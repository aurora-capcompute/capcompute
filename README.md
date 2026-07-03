# capcompute

`capcompute` is an experimental Extism-based compute runtime library — the
kernel of a small library operating system in which WebAssembly guests run as
processes and host capabilities are syscalls.

It gives a Go application a small, explicit way to run WebAssembly guests that
can call back into host-owned capabilities. The library owns the Extism plugin
lifecycle and the syscall wiring. The application owns scheduling, persistence
policy, replay policy, queues, durable state, and cleanup timing.

The intended use case is a higher-level runtime for systems such as AI agents,
controllers, and workflow-like applications where guest code can request host
work, yield, and later be resumed by the wrapping system.

See `docs/ARCHITECTURE.md` for the OS model this library implements.

## What This Library Owns

The root `capcompute` package owns:

- compiled program images (`Kernel`);
- process instances (`Process`);
- the `Process`, `PID`, and `ProcessTable` vocabulary;
- the `ProcessSpec` and `ResumeResult` lifecycle;
- the `extism:host/compute/syscall` host function;
- lookup of the current process from the configured `ProcessTable`;
- dispatching guest syscalls into a `sys.Dispatcher`.

Child packages own concrete implementations and optional strategies:

- `sys` defines `Syscall`, `SyscallResult`, and `Dispatcher`;
- `sys/replay` provides a replay dispatcher decorator;
- `sys/replay/tape/journaled` provides a journal-backed replay tape;
- `memory` provides an in-memory `ProcessTable` for tests and development.

## What This Library Does Not Own

`capcompute` deliberately does not own:

- job queues or schedulers;
- async completion;
- when a yielded process should resume;
- what value is injected back into guest logic after outside work completes;
- durable database implementations;
- when processes are saved, deleted, or closed;
- product-specific workflow, agent, or controller policy;
- program distribution or marketplace concerns.

Those belong to the system wrapping this library.

## Ambient Authority and Determinism

The kernel owns every ambient source a guest can observe. Guest module
configuration (clock, RNG, environment) is constructed inside the kernel —
pinned to deterministic sources that restart identically per instance — and
cannot be supplied by the caller. `NewKernel` rejects images that set
`allowed_hosts` or `allowed_paths` with `ErrAmbientAuthority`: network and
filesystem access are capabilities served by the dispatcher, never ambient
rights. If a guest needs real time or randomness, expose it as a journaled
capability.

## Runtime Flow

A host application usually does this:

1. Create a `Kernel` with a program image (`Config.Image`, an Extism
   manifest), a dispatcher factory, and a process table.
2. Create a `Process` from a `ProcessSpec`.
3. Save the process in the `ProcessTable` before calling `Resume` if the guest
   can make syscalls.
4. Call `Resume`.
5. Read the single `ResumeResult` from the returned handle's result channel.
6. Decide whether to save, keep, delete, recreate, or close the process.

`CreateProcess` does not save anything. This is intentional. The caller decides
when a process becomes visible to syscalls.

`Resume` invokes the configured entrypoint on the process's reusable Extism
plugin instance. It returns a `ResumeHandle` because execution happens in a
goroutine. Each handle sends exactly one `ResumeResult` and then closes its
result channel. Calling `Stop` interrupts the invocation and permanently
terminates that physical process instance.

Calling `Resume` again on a stopped process returns `ErrProcessTerminated`.
Recreate the logical process with `CreateProcess`, the same spec and PID, then
replace the old process in the table. A replay dispatcher can reuse previously
committed results from the same journal. The syscall that was active during
cancellation may execute again because it had no committed result.

Journal-backed replay records deterministic result and failed outcomes. Yield
is never committed. Dispatchers should return a Go error for cancellation or
other transient infrastructure failures that must be retried rather than
returning a deterministic failed result.

## Process Model

`Process` is the runtime object for one logical process. It contains:

- user-owned guest data;
- the original JSON input;
- the entrypoint to call;
- the Extism plugin instance;
- the dispatcher chain for syscalls.

`Process` is not thread-safe. A wrapping runtime should coordinate concurrent
use of the same process.

`PID` is implemented by user data:

```go
type Proc struct {
	ID string
}

func (p Proc) PID() string {
	return p.ID
}
```

The PID is the only tracking value carried through syscall context. The syscall
host function uses it to load the current process from the `ProcessTable`.

## ProcessTable

`ProcessTable` is the runtime lookup boundary:

```go
type ProcessTable[ID comparable, K PID[ID]] interface {
	LoadProcess(ctx context.Context, pid ID) (*Process[K], error)
	SaveProcess(ctx context.Context, pid ID, process *Process[K]) error
}
```

The table owns process visibility. If a guest makes a syscall and the process
is not in the table, the syscall returns a failed host response.

The library ships an in-memory table in `memory`; the application supplies a
durable one in production. A durable table can persist the application data
needed to recreate processes, then hydrate a new `Kernel` after a host restart
by calling `CreateProcess` and `SaveProcess` for each persistent process.

## Syscall Contract

Guests call this imported function:

```go
//go:wasmimport extism:host/compute syscall
func hostSyscall(uint64) uint64
```

The argument points to JSON matching `sys.Syscall` (`abi` must equal
`sys.ABIVersion`, currently 3; mismatches fail with code `bad_abi`):

```json
{
  "abi": 3,
  "name": "tool.name",
  "args": {"any": "json"}
}
```

The syscall host function:

1. reads the PID from context;
2. loads the process from `ProcessTable`;
3. decodes the `sys.Syscall`;
4. calls the process dispatcher;
5. returns a JSON host response to the guest.

The host response has this shape:

```json
{
  "abi": 3,
  "status": "result",
  "result": {"any": "json"}
}
```

`status` is one of:

- `result`: the syscall completed and returned JSON;
- `yield`: the host needs outside work before the guest can make progress;
- `failed`: the syscall failed — `code` then carries a machine-readable
  errno (`denied`, `expired`, `not_found`, `invalid_args`, `transient`,
  `internal`, `bad_abi`) alongside the human `message`.

Two syscall names are reserved for savepoint brackets: `sys.begin` and
`sys.commit` (`sys.SyscallBegin`/`sys.SyscallCommit`). Hosts journal them as
side-effect-free markers; on a failed-process resume the journal is forked just
past the outermost unclosed `sys.begin` so the whole declared unit
re-executes. Brackets have stack semantics.

The guest decides what to do with that response. In particular, a host `yield`
does not automatically pause the guest. The guest must return from its exported
function with the resume-result convention described below.

## Resume Result Convention

`ResumeResult.Status` is derived from the Extism call result:

- `ResumeFailed`: the guest call returned an Extism/runtime error;
- `ResumeStopped`: the resume context was cancelled and the physical process
  was terminated;
- `ResumeYielded`: the guest succeeded and returned JSON with
  `{"status":"yielded"}`;
- `ResumeCompleted`: the guest succeeded and returned JSON with
  `{"status":"completed"}`;
- `ResumeFailed`: the guest output was not valid JSON or did not contain one of
  those two explicit statuses.

This convention keeps the library minimal. Yielding only means "this resume
attempt stopped at a point chosen by the guest." The wrapping system decides
when to resume the process again and what data should be available when it
does.

## Minimal Host Setup

```go
ctx := context.Background()
// table is any ProcessTable[string, Run] the application provides (memory.NewProcessTable
// in tests, a durable table in production).
table := memory.NewProcessTable[string, Run]()

kernel, err := capcompute.NewKernel[string, Run](ctx, capcompute.Config[string, Run]{
	Image: extism.Manifest{
		Wasm: []extism.Wasm{extism.WasmFile{Path: "plugin.wasm"}},
	},
	PluginConfig: extism.PluginConfig{
		EnableWasi: true,
	},
	ProcessTable: table,
})
if err != nil {
	return err
}
defer kernel.Shutdown(ctx)

proc := Proc{ID: "proc-1"}
process, err := kernel.CreateProcess(ctx, capcompute.ProcessSpec[string, Proc]{
	Input:      json.RawMessage(`{"task":"example"}`),
	Entrypoint: "run",
	UserData:   proc,
	Dispatcher: procDispatcher{},
})
if err != nil {
	return err
}
defer process.Close(ctx)

if err := table.SaveProcess(ctx, proc.PID(), process); err != nil {
	return err
}

handle, err := kernel.Resume(ctx, process)
if err != nil {
	return err
}

result := <-handle.Results()
switch result.Status {
case capcompute.ResumeCompleted:
	// The guest finished this resume attempt.
case capcompute.ResumeYielded:
	// Keep or persist enough state for the wrapping system to resume later.
case capcompute.ResumeStopped:
	// Recreate the process before resuming this logical process.
case capcompute.ResumeFailed:
	// Inspect result.Err and apply application error policy.
}
```

The dispatcher is application code, supplied per process through
`ProcessSpec.Dispatcher`. It handles one process's syscalls:

```go
type procDispatcher struct{}

func (procDispatcher) Dispatch(ctx context.Context, proc Proc, syscall sys.Syscall, auth sys.Authorization) (sys.SyscallResult, error) {
	switch syscall.Name {
	case "echo":
		return sys.Result(syscall.Args), nil
	case "wait":
		return sys.Yield("waiting for outside work"), nil
	default:
		return sys.Fail("unknown syscall"), nil
	}
}

func (procDispatcher) Capabilities() []sys.Capability { return nil }
```

Replay dispatchers forward capability metadata from their wrapped dispatcher,
and `Process.Capabilities()` returns the capabilities of the exact process
chain.

## Guest Convention

Guests are normal Extism plugins. A Go/TinyGo guest can use
`github.com/extism/go-pdk`.

The guest imports `extism:host/compute/syscall`, sends `sys.Syscall` JSON,
reads the host response, and then either continues, returns a completed output,
returns `{"status":"yielded"}`, or sets an Extism error.

The integration fixture in `testdata/integration_guest` shows the smallest Go
guest that exercises completed, yielded, and failed resume states.

## Replay

Replay is modeled as dispatcher behavior, not as a separate root lifecycle
method. Guest code re-enters from the top. A replay dispatcher can serve
recorded results from a tape and delegate new syscalls upstream when the tape
does not contain a matching record.

The root package does not decide when replay happens or when async work is
complete. A wrapping runtime can build that policy by choosing the dispatcher
chain it creates for a process.

## Testing

Always run:

```sh
go test ./...
go vet ./...
```

There is an optional TinyGo integration test. If `tinygo` is installed, it builds
and runs the real guest fixture. If `tinygo` is not installed, the test skips.

In sandboxed environments where the default Go cache is not writable, use a
writable cache:

```sh
GOCACHE=/tmp/capcompute-go-build go test ./...
GOCACHE=/tmp/capcompute-go-build go vet ./...
```

## Extism Tooling

Extism also provides CLI and PDK tooling. The `extism` CLI can generate plugin
projects and call simple plugins. That is useful for plugin authors, but it does
not replace this library's integration tests because `capcompute` relies on a
custom host import that must be provided by Go test code.

XTP and schema-driven bindgen may be useful later if the guest/host contract is
published as a stable plugin-author API. For now, the library keeps that
contract explicit in Go types and JSON conventions.
