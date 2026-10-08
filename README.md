# capcompute

`capcompute` is a Go library that runs WebAssembly (Wasm) programs as
**processes**. A process can affect the outside world only through one host
function, the **syscall**. The host answers each syscall through a dispatcher
that the application supplies.

It is the processor of a small operating system:

- A Wasm module is a **program**.
- A running instance is a **process**.
- Host features (network, LLM calls, storage) are **syscalls**.

[`docs/ARCHITECTURE.md`](docs/ARCHITECTURE.md) describes the model and its laws.

## Where it fits

`capcompute` is the bottom layer of the Aurora repos:

```
   aurora-cli / aurora-slack-connector      clients
              │  HTTP /v1
         aurora-dist                         the server binary
              │
   ┌──────────┴──────────┐
 aurora-capcompute    aurora-dispatchers     runtime + capability drivers
   └──────────┬──────────┘
         capcompute                          the processor (this repo)

   aurora-brains                             the Wasm programs that run inside
```

- **capcompute**: isolation, the one syscall gate, deterministic execution,
  resource caps.
- **[aurora-capcompute](https://github.com/aurora-capcompute/aurora-capcompute)**:
  the runtime. It owns the reference monitor (grants, flow policy), replay, the
  journal, sessions, retries and approvals.
- **[aurora-dispatchers](https://github.com/aurora-capcompute/aurora-dispatchers)**:
  the drivers that make HTTP calls, run allowlisted commands and call an LLM.
- **[aurora-brains](https://github.com/aurora-capcompute/aurora-brains)**: the
  agent programs.
- **[aurora-dist](https://github.com/aurora-capcompute/aurora-dist)**: one server
  that assembles all of the above.

## What it gives you

| Property | Effect |
| --- | --- |
| **No ambient authority.** The syscall is the only way out: no filesystem, no network, no environment. | Code that an LLM wrote reaches only what the dispatcher serves. |
| **Deterministic execution.** The processor owns the WASI clock and random source. A fresh instance sees the same sequence. | The layer above can rebuild a process by replaying its journal. |
| **Resource caps.** A memory cap per program (`Config.MaxMemoryPages`) and a deadline per quantum (`ProcessSpec.ResumeTimeout`). | A guest cannot exhaust the host or loop forever. |
| **Yield and resume.** A syscall can yield for outside work, such as an approval or a timer. | A process can wait without holding a thread. |

The journal, replay, savepoints, grants and flow control are built on this
gate in aurora-capcompute (`monitor/`, `replay/`, `journaled/`).

## Quick start

Requirement: Go 1.26 or later. The guest tests build their Wasm fixtures with
the same `go` toolchain.

```sh
go build ./...
go vet ./...
go test ./...          # -short skips the Wasm guest tests
```

If the default Go cache is not writable, set `GOCACHE`:

```sh
GOCACHE=/tmp/capcompute-go-build go test ./...
```

This is a library. The only runnable Wasm is the test fixtures in `testdata/`.

## Lifecycle

A host uses three calls: `NewProgram`, `NewProcess` and `Resume`.

```go
// 1. Compile a program from an Extism manifest.
program, err := capcompute.NewProgram(ctx, capcompute.Config{
    Image:        extism.Manifest{Wasm: []extism.Wasm{extism.WasmFile{Path: "plugin.wasm"}}},
    PluginConfig: extism.PluginConfig{EnableWasi: true},
})
defer program.Close(ctx)

// 2. Create a process. Input, credential and dispatcher are explicit.
//    The process inherits nothing.
process, err := capcompute.NewProcess(ctx, program, capcompute.ProcessSpec[Run]{
    Input:      json.RawMessage(`{"task":"example"}`),
    Entrypoint: "run",
    Cred:       Run{ID: "proc-1"}, // host-side identity; the guest never sees it
    Dispatcher: myDispatcher{},
})
defer process.Close(ctx)

// 3. Run one quantum and read its single result.
handle, _ := capcompute.Resume(ctx, process)
result := <-handle.Results()
switch result.Status {
case capcompute.ResumeCompleted: // the guest finished
case capcompute.ResumeYielded:   // the guest waits for outside work
case capcompute.ResumeStopped:   // cancelled; create a new process to continue
case capcompute.ResumeFailed:    // apply your error policy
}
```

`Resume` binds a dispatch closure to the process's credential and dispatcher.
The host function calls that closure, so it needs no process lookup.

### The dispatcher

The application answers a process's syscalls:

```go
func (myDispatcher) Dispatch(ctx context.Context, cred Run, call sys.Syscall, auth sys.Authorization) (sys.SyscallResult, error) {
    switch call.Name {
    case "echo":
        return sys.Result(call.Args), nil
    case "wait":
        return sys.Yield("waiting for outside work"), nil
    default:
        return sys.Fail("unknown syscall"), nil
    }
}
```

The dispatcher decides what a syscall does. Policy (which operations exist,
labels, flow rules, approval) belongs to the runtime above. The processor has
no vocabulary for it.

## The syscall contract

A guest imports one host function:

```go
//go:wasmimport extism:host/compute syscall
func hostSyscall(uint64) uint64
```

The request is a `sys.Syscall` and the response is a `sys.SyscallResult`, both
as JSON (ABI version 4, `sys.ABIVersion`). The journal and the dispatcher chain
use the same types. A response has one of three statuses:

- `result`: the syscall completed and returned a value.
- `yield`: the host needs outside work before the guest can continue.
- `failed`: an errno (`denied`, `expired`, `not_found`, `invalid_args`,
  `transient`, `internal`, `bad_abi`) and a message.

The guest returns `{"status":"completed"}` or `{"status":"yielded"}` from its
exported function. The processor reserves these names: `sys.begin`,
`sys.commit`, `sys.input`, `sys.output`, `sys.log`, `sys.abort`, `sys.timer`,
`sys.declassify`, `sys.now`, `sys.random`.

## Scope

`capcompute` does not own queues, schedulers, stores, replay, async completion
or agent policy. The systems on top own them. Every `.go` file here is
processor API that something uses, or a test. Code with no consumer is
removed.

## Layout

```
processor.go   Program, Process, NewProcess, Resume
host.go        the host function and syscall dispatch
ambient.go     the deterministic clock and random source
sys/           Syscall, SyscallResult, Dispatcher, errno: the ABI v4 envelope
docs/          ARCHITECTURE.md (the model), ROADMAP.md
testdata/      Wasm guest fixtures for the integration tests
```
