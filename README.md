# capcompute

**A tiny "operating system" for running AI‑agent code safely.** `capcompute` is a
Go library that runs WebAssembly (Wasm) programs as sandboxed **processes** whose
*only* way to affect the outside world is by asking the host for permission — one
call at a time, all recorded.

> New here? Read the next two sections and you'll understand what this is and why
> it exists. Then jump to [Quick start](#quick-start-5-minutes) to build and test
> it locally.

---

## What is this, in plain words?

Imagine you let an AI agent run real commands — restart a server, charge a card,
delete a file. Three things immediately go wrong:

1. **You can't trust the logs.** The agent prints whatever it wants. You need a
   record of what it *actually did* that it can't skip or fake.
2. **Crashes double‑charge you.** It dies at step 7 of 12, and step 3 was a
   payment. You restart it… and steps 1–6 run again. You just paid twice.
3. **The agent guards its own gate.** "A human must approve deletes" lives in the
   agent's prompt — which the AI controls. The prisoner writes the prison rules.

`capcompute` is the runtime where those three have answers:

- **Every side effect goes through one recorded gate** the program can't bypass —
  an un‑forgeable audit trail.
- **Crashes replay to the exact instruction** without re‑running effects that
  already committed — no double‑charges.
- **Approval lives outside the sandbox**, where the AI can't approve for itself.

It's a **library, not an app** — you link it into your own Go program. Think of it
as the "kernel" of a small operating system: Wasm modules are **programs**, running
instances are **processes**, and host features (network, LLM calls, storage) are
**syscalls** the kernel mediates. See [`docs/ARCHITECTURE.md`](docs/ARCHITECTURE.md)
for the full OS model.

## Where this fits in the Aurora system

`capcompute` is the bottom layer of a family of repos (all under the
[`aurora-capcompute`](https://github.com/aurora-capcompute) org) that together run
AI agents safely:

```
        you (a human)
              │
   aurora-cli / aurora-slack-connector      ← clients you talk to
              │  HTTP /v1
         aurora-dist                         ← the server (one binary you run)
              │  assembled from…
   ┌──────────┼─────────────────────┐
 aurora-      aurora-dispatchers      capcompute   ◀── YOU ARE HERE
 capcompute   (capability drivers:    (the kernel)
 (orchestr.)   internet, LLM, files)
              │
        aurora-brains                        ← the agent "programs" (Wasm) that run inside
```

- **capcompute (this repo)** — the kernel: sandboxing, syscalls, the recorded
  journal, replay, capability checks.
- **[aurora-capcompute](https://github.com/aurora-capcompute/aurora-capcompute)** —
  the orchestration runtime built on top (sessions, retries, approvals, sub‑agents).
- **[aurora-dispatchers](https://github.com/aurora-capcompute/aurora-dispatchers)** —
  the concrete drivers that actually make HTTP calls, read files, call an LLM.
- **[aurora-brains](https://github.com/aurora-capcompute/aurora-brains)** — the
  Wasm agent programs (the "cognition") that run *as processes* inside this kernel.
- **[aurora-dist](https://github.com/aurora-capcompute/aurora-dist)** — bundles all
  of the above into one runnable server.

You rarely use `capcompute` on its own. It's the engine the rest is built from.

## What it does for you (features)

| Feature | The problem it solves |
| --- | --- |
| **Capability security** — a program can only call the syscalls it was explicitly granted, checked against a JSON schema | Untrusted / LLM‑written code can't reach anything you didn't hand it ("zero ambient authority") |
| **Recorded journal** — every syscall is written down *before* it runs (the intent) and *before* the guest sees the answer (the completion), hash‑chained | Tamper‑evident audit trail; the record can't be skipped or forged |
| **Deterministic replay** — clock and randomness are syscalls, pinned and replayed | A crashed process resumes at the exact instruction, seeing identical values |
| **Exactly‑once effects** — committed results are served from the journal on replay | No double‑charges after a crash or restart |
| **Savepoints + rollback (sagas)** — `sys.begin`/`sys.commit` brackets, `sys.compensate` undo actions, `sys.abort` to unwind | Partial work can be cleanly reversed and retried |
| **Yield / resume** — a process can pause on outside work (an approval, a timer) and be resumed later | Human‑in‑the‑loop and long waits without holding a thread |
| **Information‑flow control** — results carry provenance **labels**; a **flow monitor** refuses a call whose inputs are too "tainted" | Stops sensitive data (or prompt‑injected content) from flowing into dangerous actions |
| **Child processes** — `sys.spawn` starts sub‑programs with *attenuated* authority (a child can't be granted more than its parent holds) | Safe delegation to sub‑agents |
| **Fair scheduling & supervision** — priority bands, per‑owner quotas, virtual‑actor residency, OTP‑style restarts | Many tenants share the runtime without starving each other |

## Quick start (5 minutes)

**Prerequisites:** Go 1.26+. (Optional: [TinyGo](https://tinygo.org) to run the
end‑to‑end guest tests — they auto‑skip if it's missing.)

```sh
git clone https://github.com/aurora-capcompute/capcompute
cd capcompute

go build ./...      # compile everything
go vet ./...        # static checks
go test ./...       # run the test suite
```

Running in a sandbox where the default Go cache isn't writable? Point it somewhere
writable:

```sh
GOCACHE=/tmp/capcompute-go-build go test ./...
```

Run only the fast unit tests, or a single package:

```sh
go test -short ./...                      # skip the slow TinyGo integration test
go test ./sys/replay/tape/journaled/      # one package
```

If you have TinyGo installed, the integration test builds a real Wasm guest and
drives it through completed / yielded / failed / crash‑replay states:

```sh
go test -run TestTinyGoGuest ./...
# internally it builds the guest fixture with:
#   tinygo build -target wasip1 -buildmode=c-shared -tags tinygo \
#     -o guest.wasm ./testdata/integration_guest
```

There is **no `go run`** — this is a library. The only runnable Wasm artifacts are
the guest fixtures under `testdata/`, and the Go tests build and drive them.

## How you use it (the lifecycle)

A host application wraps the kernel like this:

1. **Create a kernel** from a program image (an Extism Wasm manifest) and a
   process table:

   ```go
   kernel, err := capcompute.NewKernel[string, Run](ctx, capcompute.Config[string, Run]{
       Image:        extism.Manifest{Wasm: []extism.Wasm{extism.WasmFile{Path: "plugin.wasm"}}},
       PluginConfig: extism.PluginConfig{EnableWasi: true},
       ProcessTable: table, // you supply this; the interface is below
   })
   defer kernel.Shutdown(ctx)
   ```

2. **Create a process** from a `ProcessSpec` (its input, entrypoint, credential,
   and syscall dispatcher):

   ```go
   process, err := kernel.CreateProcess(ctx, capcompute.ProcessSpec[string, Run]{
       Input:      json.RawMessage(`{"task":"example"}`),
       Entrypoint: "run",
       Cred:       Run{ID: "proc-1"}, // host-side identity, never visible to the guest
       Dispatcher: myDispatcher{},    // handles this process's syscalls
   })
   ```

3. **Save it** into the process table (this makes it visible to syscalls), then
   **resume** it and read the single result:

   ```go
   _ = table.SaveProcess(ctx, "proc-1", process)
   handle, _ := kernel.Resume(ctx, process)
   result := <-handle.Results()
   switch result.Status {
   case capcompute.ResumeCompleted: // the guest finished
   case capcompute.ResumeYielded:   // paused on outside work — resume later
   case capcompute.ResumeStopped:   // cancelled — recreate before resuming
   case capcompute.ResumeFailed:    // apply your error policy
   }
   ```

`CreateProcess` does **not** save anything — *you* decide when a process becomes
visible. `Resume` runs the guest in a goroutine and delivers exactly one
`ResumeResult`.

### The two pieces you provide

**A dispatcher** — application code that answers one process's syscalls:

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
func (myDispatcher) Capabilities() []sys.Capability { return nil }
```

**A process table** — the kernel's lookup boundary for live processes. The library
ships only the interface; you provide the implementation (a durable one in
production; the tests use an in‑memory double):

```go
type ProcessTable[ID comparable, K PID[ID]] interface {
    LoadProcess(ctx context.Context, pid ID) (*Process[K], error)
    SaveProcess(ctx context.Context, pid ID, process *Process[K]) error
}
```

## The syscall contract (guest ↔ host)

A guest imports one host function and sends it a request:

```go
//go:wasmimport extism:host/compute syscall
func hostSyscall(uint64) uint64
```

The request and response are an **ABI‑v3 protobuf envelope** (`sys.ABIVersion == 3`;
the wire codec lives in `sys/wire`). A response has one of three statuses:

- `result` — the syscall completed and returned a value;
- `yield` — the host needs outside work before the guest can continue;
- `failed` — carrying a machine‑readable errno (`denied`, `expired`, `not_found`,
  `invalid_args`, `transient`, `conflict`, `internal`, `bad_abi`) plus a message.

The guest decides what to do with the response and returns from its exported
function with `{"status":"completed"}` or `{"status":"yielded"}`. Some syscall
names are reserved by the kernel: `sys.begin`, `sys.commit`, `sys.compensate`,
`sys.abort`, `sys.spawn`, `sys.timer`, `sys.declassify`, `sys.now`, `sys.random`.

## What this library deliberately does **not** own

`capcompute` is intentionally small. It does *not* own job queues, schedulers of
*when* to resume, durable databases, async completion, or product‑specific agent
policy. Those belong to the system wrapping it — that's what
[aurora-capcompute](https://github.com/aurora-capcompute/aurora-capcompute) and
[aurora-dist](https://github.com/aurora-capcompute/aurora-dist) are.

## Project layout

```
kernel.go        Kernel, Process, ProcessTable, the Resume lifecycle
host.go          the single Extism host function + syscall dispatch
stack.go         Stack.ForProcess — the canonical dispatcher chain order
validate.go      Validator: the reference monitor (grants + arg schemas)
provenance.go    labels, taints, flow monitor, declassifier (data-flow control)
ambient.go       deterministic clock + RNG (so replay is exact)
spawn.go         sys.spawn: child processes with attenuated authority
throttle.go      rate limiting (delays, never denies)
sys/             the syscall vocabulary: Syscall, Dispatcher, Capability, errno
  replay/        replay decorator + journal-backed tape (the WAL / audit log)
  wire/          the ABI-v3 protobuf envelope codec (shared with guests)
sched/           fair-share scheduler + OTP-style supervisor
sim/             deterministic fault-injection simulation harness
otelexport/      render a journal as OpenTelemetry traces
docs/            ARCHITECTURE.md (the OS model), PITCH.md, ROADMAP.md, …
testdata/        the smallest TinyGo guest fixtures used by integration tests
```

## Related repos

- [aurora-capcompute](https://github.com/aurora-capcompute/aurora-capcompute) — the orchestration runtime built on this kernel
- [aurora-dispatchers](https://github.com/aurora-capcompute/aurora-dispatchers) — the capability drivers
- [aurora-brains](https://github.com/aurora-capcompute/aurora-brains) — the Wasm agent programs that run inside
- [aurora-dist](https://github.com/aurora-capcompute/aurora-dist) — the runnable server that bundles it all
- [aurora-cli](https://github.com/aurora-capcompute/aurora-cli) — the terminal client
