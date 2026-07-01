# capcompute — Kernel Architecture (the OS model)

This document is the **design north star** for capcompute. It describes the system
as what it structurally is: a small **operating system for WebAssembly guests**.
The metaphor is not decoration — it is a build discipline. When a new feature is
proposed, the first question is *"what OS concept is this?"*, because that question
comes with 50 years of prior art about how to get it right.

Use this as the shared vocabulary for both human and LLM-assisted development.

## What this is (honest classification)

capcompute is a **library operating system** — OS abstractions provided as a Go
library linked into a host application, not a standalone kernel on hardware. It is:

- **capability-based** — guests have *zero ambient authority*; they can only invoke
  explicitly granted capabilities (lineage: seL4, KeyKOS);
- **cooperatively scheduled** — guests run to `yield`/completion; there is no
  preemption (lineage: classic cooperative multitasking);
- **orthogonally persistent** — no filesystem; process state is reconstructed by
  replaying a journal of host calls (lineage: single-level store — AS/400, KeyKOS,
  EROS);
- **durably replayable** — the journal doubles as crash-recovery *and* audit trail
  (lineage: durable execution — Temporal; VM replay — ReVirt).

Closest living relatives: the **Erlang/OTP BEAM** (an OS-like runtime with
processes, supervision, message passing) and **Golem** (wasm + durable replay).
This is a real, identifiable design point — not a novel one. Where a decision is
open, port it from that prior art rather than reinventing it.

## Glossary (code name → OS concept → contract)

The left column is the current code; the middle is the OS concept it *is*; the
right is the promise the name makes. Renames adopt the OS name **only where the
thing honors that concept's contract** — never introduce a false friend
(no `Thread`: there is no preemption; no `Interrupt`: yields are cooperative).

| Code today | OS concept | Contract |
|---|---|---|
| `ComputeCompiledPlugin` | **Kernel** (bound to one program image) | owns the process table, wires the syscall entry, spawns processes |
| wasm module ("brain") | **Program** (executable image) | on-disk code; many processes may run one program |
| `Session` | **Process** | one running instance; states idle/active/terminated ≈ ready/running/terminated |
| `SessionKey` / `.SessionKey()` | **PID** | stable, deterministic process identity |
| `SessionStore` (`LoadSession`/`SaveSession`) | **Process table** | the kernel's lookup boundary for live processes |
| `Play` / `PlayRequest` / `PlayResult` / `PlayHandle` | **Run / schedule a quantum** | give a process CPU; outcome is yielded/completed/stopped/failed |
| `dispatcher.Call{Name,Args}` | **Syscall** (request) | the guest→host request crossing the trap boundary |
| `dispatcher.Outcome{result\|yield\|failed}` | **Syscall result** | the value/effect returned to the guest |
| `dispatcher.Capability` | **Capability** | keep the name — already the exact security term |
| `dispatcher.Authorization` | **Grant / approval context** | forward-propagated authority for a replayed action |
| `dispatcher.Dispatcher` | **Syscall handler / driver interface** | turns a `Call` into an `Outcome`; lists `Capabilities()` |
| concrete dispatchers (`aurora-dispatchers`) | **Drivers** (outbound) | mediate a process's I/O to external devices |
| chat sources (Telegram/Slack) | **Drivers** (inbound) + **controlling terminal** | see *Drivers: the symmetry* |
| replay journal / tape | **Journal** (WAL) | append-only log = durability + audit (one structure, two jobs) |

> Package note: do **not** name a package `syscall` (shadows Go's stdlib). Use
> `abi`, `sys`, or fold syscall types into a `kernel` package.

## The syscall ABI

The guest↔host boundary is a single Extism host function, namespace
`extism:host/compute`, function `play`. The guest emits a `Call`; the kernel loads
the current process from the process table (by PID carried in context), dispatches
the `Call` through that process's driver chain, and returns an `Outcome`.

```
Syscall  (Call):    { "name": string, "args": <json> }
Result   (Outcome): { "status": "result" | "yield" | "failed",
                      "result": <json>,   // when result
                      "message": string } // when yield/failed
```

Guest programs return `{"status":"completed",...}` or `{"status":"yielded"}` from
their entrypoint. **This ABI is your POSIX: version it, keep it small, and treat
changes as breaking.** It is the contract every driver and every LLM-generated
component builds against.

## The five invariants (kernel laws)

These must always hold. Encode them as tests/CI checks; they are what make the
governance and durability claims *provable* rather than aspirational.

1. **No ambient authority.** A process can do nothing except call granted
   capabilities. (This is the crown jewel — the moment a guest is given ambient
   WASI/host access "for convenience," enforcement degrades to advisory.)
2. **Determinism.** Guests are deterministic; *all* non-determinism (clock,
   randomness, I/O) flows through syscalls. No wall-clock or RNG inside a guest.
3. **Journal-before-observe.** Every side-effecting syscall's outcome is committed
   to the journal before it is observable, so replay is exact. (`yield` is never
   committed — it is a re-triable, blocking syscall.)
4. **Un-bypassable reference monitor.** An approval-required capability cannot
   execute without a resolved `Authorization`.
5. **Minimal TCB.** The kernel owns lifecycle, syscall dispatch, and enforcement —
   nothing else. Guard the boundary; helpers do not belong in the kernel.

## Drivers: the symmetry

Drivers are the one category that mediates between processes and the outside world.
They come in two directions, and recognizing them as *the same category pointed
opposite ways* keeps the architecture coherent:

- **Outbound drivers = dispatchers.** Called *by* a process as an outbound syscall
  (`process → device`): internet reads, MCP tools, k8s/Helm, `timer.set`. The
  process initiates; the driver mediates access to a machine device.
- **Inbound drivers = sources.** Drive processes *from outside* (`human → kernel →
  process`): Telegram, Slack. The device on the other end is a **human**. A source
  fuses three classic roles — `getty` (accepts a "login" = a user starting a
  conversation), the **tty** (streams messages in/out), and the process's
  **controlling terminal**.

Consequences of the symmetry, used as design rules:

- A new integration is a **driver**; decide its direction (does a process call it,
  or does it drive processes?) and it slots into the existing model.
- **Human approval is terminal I/O.** `require_approval` is a process writing a
  prompt to its controlling terminal and performing a *blocking read* for the
  reply. It composes with the yield/resume model: the read is a `yield` until the
  human answers.
- **Commands are job control.** `/cancel` = Ctrl-C (SIGINT to the foreground
  process = `Stop`), `/status` = job status, `/retry` = resume, `/new` = new
  session. Conversation↔thread is the *controlling terminal ↔ session* binding.
- Unlike a classic byte-stream tty, sources are **durable and multiplexed**: the
  inbox is persisted before the poll offset advances (idempotent input across
  crashes), sessions survive restart. Model it as a persistent terminal server
  (getty + tmux that survives reboot), consistent with orthogonal persistence.

## Processes and `spawn` (planned syscall)

Agents creating agents is the **`spawn` syscall**. Design decisions, with prior art:

- **`spawn`, not `fork`.** Use `posix_spawn`/`CreateProcess` semantics:
  `spawn(program, input, capabilities) → child_pid`. A *fresh* process running a
  named program with *explicitly handed* capabilities. Do **not** clone parent
  state (POSIX `fork` is a known design mistake to copy — see *"A fork() in the
  road"*, HotOS '19; and cloning would mean cloning the journal).
- **Capability delegation with attenuation.** The child's capabilities must be a
  subset of what the parent holds (KeyKOS/seL4). The parent cannot grant authority
  it lacks. The `spawn` call is journaled, so the whole process tree's authority
  graph is auditable for free.
- **Deterministic child PID.** Derive it — `child_pid = f(parent_pid, spawn_seq,
  program)` with a per-parent monotonic `spawn_seq`. Never random (invariant #2).
- **Synchronous first.** v1: parent `spawn`s, yields until the child completes,
  reads the child's result from *its own* journal on replay (child is not re-run).
  This is the "child workflow" pattern. Defer async/concurrent spawn — it requires
  journaling every inter-process message as an ordered input event (actor model +
  event sourcing), a real determinism cost to pay only when concurrency is needed.
- **Two host-side contracts, not just the guest ABI:** (a) *scheduler hand-off* —
  `spawn`'s host handler enqueues the new `Session` into the wrapping runtime's
  queue (scheduling lives in userland, by design); (b) *supervision* — decide
  cascade-kill vs orphan-adopt on parent `Stop`. Study Erlang/OTP supervision trees
  before this grows.

Implementation sketch: a new `dispatcher.Call` (`process.spawn`) handled by a
builtin driver whose handler calls `CreateSession` + `SaveSession`, drives or
enqueues the child, and commits the child's result into the parent's replay tape.

## Persistence and replay

There is no filesystem. A process is durable because the kernel journals every
syscall outcome; after a crash the kernel recreates the process from its persisted
program + input and **replays the journal** to the exact interruption point. The
same append-only journal is the **audit trail** — every input, effect, capability
grant, and approval decision, in order. Durability and audit are one mechanism, by
design (the write-ahead-log pattern: one log, crash-recovery + history).

## Non-goals (resist gold-plating)

The metaphor is a map, not a checklist. Implement an OS concept only when a real
requirement forces it. Not planned unless needed: preemptive scheduling, virtual
memory/paging, a POSIX filesystem, signals beyond cancel, and async multi-process
IPC. A uniprocessing, cooperatively-scheduled OS is still an OS.
