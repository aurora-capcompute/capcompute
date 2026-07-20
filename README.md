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

`capcompute` is the kernel those answers are built on:

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
   ┌──────────┴──────────┐
 aurora-capcompute    aurora-dispatchers     ← orchestration runtime + capability drivers
   └──────────┬──────────┘
              │  both built on
         capcompute                          ◀── YOU ARE HERE (the kernel)

   aurora-brains  →  the agent "programs" (Wasm) that run inside
```

- **capcompute (this repo)** — the processor: sandboxing, the one syscall gate,
  deterministic execution, resource caps.
- **[aurora-capcompute](https://github.com/aurora-capcompute/aurora-capcompute)** —
  the orchestration runtime built on top: the reference monitor (grants, flow
  policy), replay, the journal, sessions, retries, approvals, sub‑agents.
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
| **One syscall gate, zero ambient authority** — a program's only way to affect anything is the host syscall; no filesystem, no network, no env | Untrusted / LLM‑written code can't reach anything you didn't wire into its dispatcher |
| **Deterministic execution** — the WASI clock and RNG are kernel‑pinned; a fresh instance observes the identical sequence | The layer above can rebuild a crashed process by replaying its journal to the exact instruction |
| **Resource caps** — per‑process memory limit and resume deadline | A hostile or buggy guest can't exhaust the host or spin forever |
| **Yield / resume** — a process can pause on outside work (an approval, a timer) and be resumed later | Human‑in‑the‑loop and long waits without holding a thread |

The governed‑execution features built *on* this gate — the recorded
hash‑chained journal, exactly‑once replay, savepoints and compensation,
capability grants and information‑flow control — live in
[aurora-capcompute](https://github.com/aurora-capcompute/aurora-capcompute)
(its `monitor`, `replay`, and `journaled` packages).

## Quick start (5 minutes)

**Prerequisites:** Go 1.26+. Nothing else — the end‑to‑end guest tests build
their Wasm fixtures with the same `go` that runs them.

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
go test -short ./...                      # skip the slow Wasm guest tests
go test ./sys/replay/tape/journaled/      # one package
```

The integration test builds a real Wasm guest and drives it through completed /
yielded / failed / crash‑replay states:

```sh
go test -run TestGuest ./...
# internally it builds the guest fixture with:
#   GOOS=wasip1 GOARCH=wasm go build -buildmode=c-shared \
#     -o guest.wasm ./testdata/integration_guest
```

There is **no `go run`** — this is a library. The only runnable Wasm artifacts are
the guest fixtures under `testdata/`, and the Go tests build and drive them.

## How you use it (the lifecycle)

A host application uses three calls — `NewProgram`, `NewProcess`, `Resume`:

1. **Compile a program** from an image (an Extism Wasm manifest):

   ```go
   program, err := capcompute.NewProgram(ctx, capcompute.Config{
       Image:        extism.Manifest{Wasm: []extism.Wasm{extism.WasmFile{Path: "plugin.wasm"}}},
       PluginConfig: extism.PluginConfig{EnableWasi: true},
   })
   defer program.Close(ctx)
   ```

2. **Instantiate a process** from it (`posix_spawn` semantics: explicit input,
   credential, and syscall dispatcher — nothing inherited):

   ```go
   process, err := capcompute.NewProcess(ctx, program, capcompute.ProcessSpec[Run]{
       Input:      json.RawMessage(`{"task":"example"}`),
       Entrypoint: "run",
       Cred:       Run{ID: "proc-1"}, // host-side identity, never visible to the guest
       Dispatcher: myDispatcher{},    // handles this process's syscalls
   })
   defer process.Close(ctx)
   ```

3. **Resume** it and read the single result:

   ```go
   handle, _ := capcompute.Resume(ctx, process)
   result := <-handle.Results()
   switch result.Status {
   case capcompute.ResumeCompleted: // the guest finished
   case capcompute.ResumeYielded:   // paused on outside work — resume later
   case capcompute.ResumeStopped:   // cancelled — spawn afresh before resuming
   case capcompute.ResumeFailed:    // apply your error policy
   }
   ```

`Resume` runs the guest in a goroutine and delivers exactly one
`ResumeResult`; the process it planted in the call context is the one the
syscall host function dispatches through — there is no other lookup.

### The one piece you provide

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

## The syscall contract (guest ↔ host)

A guest imports one host function and sends it a request:

```go
//go:wasmimport extism:host/compute syscall
func hostSyscall(uint64) uint64
```

The request and response are an **ABI‑v4 JSON envelope** (`sys.ABIVersion == 4`):
a `sys.Syscall` in, a `sys.SyscallResult` out — the same types, and the same
JSON, that the dispatcher chain and the journal already speak. A response has
one of three statuses:

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
*when* to resume, durable databases, async completion, exporters, or
product‑specific agent policy. Those belong to the system wrapping it — that's
what [aurora-capcompute](https://github.com/aurora-capcompute/aurora-capcompute)
and [aurora-dist](https://github.com/aurora-capcompute/aurora-dist) are.
The rule is visible in the tree: every `.go` file here is either consumed
kernel API or a `_test.go` file — built‑ahead code with no consumer gets
removed (design kept in docs) until a consumer forces it back.

## Project layout

```
processor.go     Program, Process, NewProcess, Resume — the processor
host.go          the single Extism host function + syscall dispatch
ambient.go       deterministic clock + RNG (so replay above is exact)
sys/             the syscall vocabulary: Syscall, Dispatcher, Capability, errno
                 — and, via their JSON tags, the ABI-v4 wire envelope itself
docs/            ARCHITECTURE.md (the OS model), PITCH.md, ROADMAP.md, …
testdata/        the smallest Wasm guest fixtures used by integration tests
```

## Related repos

- [aurora-capcompute](https://github.com/aurora-capcompute/aurora-capcompute) — the orchestration runtime built on this kernel
- [aurora-dispatchers](https://github.com/aurora-capcompute/aurora-dispatchers) — the capability drivers
- [aurora-brains](https://github.com/aurora-capcompute/aurora-brains) — the Wasm agent programs that run inside
- [aurora-dist](https://github.com/aurora-capcompute/aurora-dist) — the runnable server that bundles it all
- [aurora-cli](https://github.com/aurora-capcompute/aurora-cli) — the terminal client
