## What this is

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

## Glossary (code name → OS concept → contract)

The left column is the code (the OS vocabulary is now the API, as of the
rename pass); the middle is the OS concept it *is*; the right is the promise
the name makes. A name adopts an OS term **only where the thing honors that
concept's contract** — never introduce a false friend (no `Thread`: there is
no preemption; no `Interrupt`: yields are cooperative).

| Code | OS concept | Contract |
|---|---|---|
| `Kernel` | **Kernel** (bound to one program image) | owns the process table, wires the syscall entry, spawns processes |
| wasm module | **Program** (executable image) | on-disk code; many processes may run one program |
| `Process` | **Process** | one running instance; states idle/active/terminated ≈ ready/running/terminated |
| `PID` interface / `.PID()` | **PID** | stable, deterministic process identity (same type/method-name pattern as Go's `error`) |
| `ProcessTable` (`LoadProcess`/`SaveProcess`) | **Process table** | the kernel's lookup boundary for live processes |
| `Resume` / `ProcessSpec` / `ResumeResult` / `ResumeHandle` | **Schedule a quantum** | give a process CPU until it yields/completes/stops/fails (cooperative) |
| `sys.Syscall{Name,Args}` | **Syscall** (request) | the guest→host request crossing the trap boundary |
| `sys.SyscallResult{result\|yield\|failed}` | **Syscall result** | the value/effect returned to the guest |
| `sys.Capability` | **Capability** | the exact capability-security term |
| `sys.Authorization` | **Grant / approval context** | forward-propagated authority for a replayed action |
| `Process.Cred` / `Dispatch(ctx, cred, …)` | **Credential** | host-side identity + authority context for the process; never guest-visible or guest-supplied |
| `sys.Dispatcher` | **Syscall dispatcher / driver interface** | turns a `Syscall` into a `SyscallResult`; lists `Capabilities()` |
| concrete dispatchers (`aurora-dispatchers`) | **Drivers** (outbound) | mediate a process's I/O to external devices |
| chat sources (Telegram/Slack) | **Drivers** (inbound) + **controlling terminal** | see *Drivers: the symmetry* |
| `journaled.Record` (intent/completion, hash-chained) | **Journal** (WAL, intent logging) | append-only envelope+payload records = durability + audit + idempotency (one structure, three jobs) |
| `Validator` / `FlowMonitor`+`Taints` | **Reference monitor** | complete mediation: grant set, arg schemas, information flow — every access checked, none journaled (denials re-derive on replay) |
| `Stack` | **The canonical chain** | encodes which layers sit above vs below the replay boundary — the load-bearing order, in code not prose |
| `Spawner` / `sys.spawn` | **spawn(2)** | child processes with attenuated authority; deterministic child identity from the intent key |
| `sched.Scheduler` | **Scheduler** | fair share across owners, priority bands, quota backpressure; virtual-actor residency (the instance is cache, the journal is the process) |
| `sched.Supervisor` | **OTP supervision** | restart = stop + resubmit; replay makes restarts lose nothing |
| `Throttle`+`RateLimit` | **Resource limits** (aggregate) | delays, never denies — a wall-clock refusal would be guest-visible nondeterminism |
| `core.memory` (aurora-dispatchers) | **Filesystem / `$HOME`** | tenant-scoped, mount-scoped (`process` / `session` / `shared` spaces named by a separate `space` field — no tenant-wide scope, cross-tenant impossible), provenance-labelled, versioned (CAS) shared state |
| `core.scratch` (aurora-dispatchers) | **tmpfs / `/tmp`** | same operations as core.memory but a fresh per-process store — ephemeral, private to one process, never durable or shared (the home for a large read offloaded out of the model's context) |

> Package note: the syscall vocabulary lives in package `sys`, not `syscall`
> (which would shadow Go's stdlib).

## The syscall ABI

The guest↔host boundary is a single Extism host function, namespace
`extism:host/compute`, function `syscall`. The guest emits a `Syscall`; the
kernel loads the current process from the process table (by PID carried in
context), dispatches the `Syscall` through that process's driver chain, and
returns a `SyscallResult`.

```
Syscall  (proto3):  abi=1 (uint32, =3) · name=2 (string) · args=3 (bytes, JSON)
Response (proto3):  abi=1 · status=2 (result|yield|failed) · code=3 (errno)
                    · result=4 (bytes, JSON) · message=5 · labels=6 (repeated)
```

Since v3 the envelope is protobuf (`sys/wire/envelope.proto` — the interop
source of truth). The codec is hand-rolled and reflection-free (~200
dependency-free lines shared by the host and the Go program; the Rust program
mirrors it), which is what makes it TinyGo-safe without dragging
`protoreflect` into guests; interop is pinned against protoc-generated
reference code, cross-language golden fixtures, and an unknown-field-skipping
test (the schema-evolution contract: add fields freely, never reuse a
number). The `args`/`result` payloads stay opaque JSON — the envelope stays
the one uniform shape generic interposition needs. Journal records keep
canonical-JSON encoding: the wire and the store encoding are separate
concerns, and the journal is the human-readable audit path.

The envelope is versioned (`sys.ABIVersion`); the host rejects mismatches with
errno `bad_abi` (a JSON envelope is classified as the pre-v3 wire, not as
garbage). Failures carry a machine-readable errno alongside the human
message so guests branch on a closed set instead of parsing prose. Two names
are reserved for **redo scopes** — `sys.begin` / `sys.commit`
(`sys.SyscallBegin`/`sys.SyscallCommit`), journaled as side-effect-free
markers with stack semantics; failed-process resume forks the journal past the
outermost unclosed bracket. Call them what they are: a redo scope can only
*re-execute* its contents, never undo them — bracketing non-idempotent,
un-keyed effects amplifies at-least-once execution (RESEARCH.md finding 9).
The undo layer is separate and guest-authored: right after an effect the guest
registers its inverse with `sys.compensate` (a deferred syscall — journaled
with concrete args, not executed), and `sys.abort` rolls the open scope back —
the runtime executes the registered compensations newest-first, each journaled
(intent/completion, idempotency-keyed, crash-resumable), then retries the scope
after the abort's declared delay or stops. Capabilities carry no compensation
metadata: they are access control, and the undo story lives in the log, in
guest-supplied terms. Replay refuses to run past a compensation record
(`ProcessUnwoundError`) — a rolled-back tail never replays as if live.

Guest programs return `{"status":"completed",...}` or `{"status":"yielded"}` from
their entrypoint. **This ABI is your POSIX: version it, keep it small, and treat
changes as breaking.** It is the contract every driver and every LLM-generated
component builds against.

Host-side, every dispatch carries the **syscall triad**: `cred` (*who* — the
host-side credential for the process; the guest never sees or supplies it),
`syscall` (*what* is being asked), and `auth` (*what has been granted* for this
specific call — the resolved approval context). Driver stratification follows
from the triad: **leaf drivers that only perform work ignore `cred`; only
policy decorators (validation, approval, quotas) consume it.** A leaf driver
reading `cred` to make an authority decision is a layering smell — that
decision belongs in a decorator in front of it.

## The five invariants (kernel laws)

These must always hold. Encode them as tests/CI checks; they are what make the
governance and durability claims *provable* rather than aspirational.

1. **No ambient authority.** A process can do nothing except call granted
   capabilities. (This is the crown jewel — the moment a guest is given ambient
   WASI/host access "for convenience," enforcement degrades to advisory.)
   *Enforced in code:* `NewKernel` rejects images with `allowed_hosts`/
   `allowed_paths` (`ErrAmbientAuthority`); guest module config is
   kernel-constructed, never caller-supplied (`ambient.go`).
2. **Determinism.** Guests are deterministic; *all* non-determinism (clock,
   randomness, I/O) flows through syscalls. No wall-clock or RNG inside a guest.
   *Enforced in code:* the WASI clock and RNG a guest can reach are pinned to
   deterministic sources that restart identically per instance (`ambient.go`),
   so a crash-replay observes the original sequence.
3. **Journal write-ahead.** Two laws, one per direction of the boundary:
   **journal-before-execute** — an *intent* record is appended before a
   syscall's driver runs, so nothing changes the world without a trace; a
   crash between execute and commit leaves a detectable *open intent*, and
   the intent identity `(process, position, call-hash)` is the idempotency key
   handed to the driver, stable across retries. **Journal-before-observe** —
   the *completion* record is appended before the result becomes observable
   to the guest, so replay is exact. (`yield` is never committed — it is a
   re-triable, blocking syscall whose intent stays open while the external
   task is pending.)
   *Enforced in code:* the intent/completion tape
   (`sys/replay/tape/journaled`); an open intent met on replay is retried
   under its original idempotency key or surfaced as `IndeterminateError`,
   per `OpenIntentPolicy`; records are hash-chained (`prev_hash`,
   `journaled.Verify`) so the journal is tamper-evident.
4. **Un-bypassable reference monitor.** An approval-required capability cannot
   execute without a resolved `Authorization`, and the monitor validates
   *every* access — **complete mediation**: a syscall is checked against the
   cred's grant set (ungranted name → `denied`) and its args against the
   capability's declared `InputSchema` (malformed → `invalid_args`) before any
   driver sees it.
   The monitor also tracks **information flow** (the CaMeL architecture as
   a kernel primitive): each granted operation declares the source classes its
   results carry (`labels`, e.g. `untrusted_web`) and the classes that may not
   flow into it (`taints`) — inline per-operation policy the driver enforces on
   each call, since it alone decodes which operation the call args select;
   because the guest is opaque, flow is judged
   conservatively — every label a process observes taints everything it later
   emits. Declassification is the reserved `sys.declassify` syscall: every
   crossing names its labels and a reason, requires a human approval (there
   is no unapproved path — an unattended declassify would just be flow-policy
   bypass), and is journaled, so replay re-applies the crossing without
   asking again.
   *Enforced in code:* the `Validator` decorator (`validate.go`) at the front
   of the dispatcher chain, plus the provenance set (`provenance.go`) —
   `Labeler` and `Declassifier` below the replay layer (so labels and
   approved crossings are journaled) and `FlowMonitor` above it (so a
   crash-restarted host rebuilds taint exactly from replayed results, and
   replayed declassifications lift labels in order). The monitor also hands
   the process's taint downstream (`sys.Taint`) so drivers that store
   guest-derived data persist its provenance. Chain order: `Validator` →
   `Throttle` → `FlowMonitor` → replay → `Labeler` → `Declassifier` →
   `Spawner` → drivers — encoded in `Stack.ForProcess`
   (`stack.go`), so assembling a chain with a layer on the wrong side of the
   replay boundary is a construction you cannot express, not a rule you must
   remember. Reserved
   markers (`sys.begin`/`sys.commit`) are exempt because they are kernel
   control syscalls, not capabilities; `sys.declassify` is *not* exempt — it
   must be granted like any capability.
5. **Minimal TCB.** The kernel owns lifecycle, syscall dispatch, and enforcement —
   nothing else. Guard the boundary; helpers do not belong in the kernel.

## Drivers: the symmetry

Drivers are the one category that mediates between processes and the outside world.
They come in two directions, and recognizing them as *the same category pointed
opposite ways* keeps the architecture coherent:

- **Outbound drivers = dispatchers.** Called *by* a process as an outbound syscall
  (`process → device`): internet reads, k8s/Helm, `sys.timer`. The
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
  session. A conversation *is* a session; its channel is the controlling terminal.
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
  `spawn`'s host handler enqueues the new `Process` into the wrapping runtime's
  queue (scheduling lives in userland, by design); (b) *supervision* — decide
  cascade-kill vs orphan-adopt on parent `Stop`. Study Erlang/OTP supervision trees
  before this grows.

**Implemented** (`spawn.go`): the `Spawner` decorator serves the reserved
`sys.spawn` syscall *below* the parent's replay layer, so a completed spawn is
journaled like any syscall — replay serves the child's result without
re-spawning. Requested capability names are resolved against the parent's
grant set and pass `sys.Attenuate` (escalation → `denied`). The child cred is
derived from the spawn's **idempotency key** — the intent identity
`(process, position, call-hash)` — which is strictly stronger than the sketched
`spawn_seq` counter: it is stable across crash-retries *and* re-entries, so a
yielded child (transitive yield: child yields → parent's spawn yields) is
re-found by deriving the same child and replaying its own journal. Child
execution goes through the `ChildRunner` seam; `KernelChildRunner` is the
kernel-backed implementation (create → register in the process table → resume
→ close; the journal is the durable child, the instance is per-quantum). A
stopped child returns a host error so the spawn intent stays open —
outcome unknown, resolved on replay. Sync-first needs no scheduler: the child
borrows the parent's quantum by construction.

## Scheduling: the seam and the default

The `sched` package splits the concern three ways: the scheduler decides
*when* a process gets the CPU, the app decides *what* runs (`Activate` — for
a durable process, journal replay), and the kernel decides *how* (`Resume`). The
default is a fair-share scheduler: strict priority bands (High/Normal/Low),
round-robin across *owners* (the aggregation key named at `Submit`, typically
the tenant) inside a band, and per-owner concurrency quotas — the aggregate
half of resource control — enforced as **backpressure, never rejection**
(excess work waits; nothing fails because a neighbor is busy). Residency is
virtual-actor shaped (Orleans/Golem): a process activates on demand, a
yielded process stays warm, the least-recently-used idle process deactivates
when residency exceeds its bound, and a terminated one deactivates
immediately — the journal is the durable process, the instance is cache.

The syscalls-per-second half is the `Throttle` dispatcher decorator: a
per-key token bucket that only ever *delays*, never denies — a
wall-clock-dependent refusal would be guest-visible nondeterminism, while a
delay is invisible to a guest with no ambient clock (law #2 shapes even the
rate limiter).

## Supervision

Sync-first `spawn` covers composition; crash-recovery of *child* processes is
supervision (`sched/supervisor.go`): OTP strategies adapted to durable
  cooperative processes — "restart" means stop the quantum (`Scheduler.Stop`: a
  queued quantum dequeues, a running one has its context cancelled and the
  kernel kills the guest) and resubmit; replay reconstructs the child
  exactly, so a restart loses no committed work. `one-for-one` /
  `one-for-all` / `rest-for-one`, with supervisor-wide restart intensity
  (max restarts per window; exceeded → give up and escalate through
  `OnExit`). Failures burn intensity; strategy-triggered sibling restarts do
  not. Completed and yielded processes exit supervision normally — crashes are
  what supervision is for.

## Persistence and replay

A process's *own* state has no filesystem: a process is durable because the
kernel journals every syscall outcome; after a crash the kernel recreates the
process from its persisted program + input and **replays the journal** to the
exact interruption point. The same append-only journal is the **audit trail** —
every input, effect, capability grant, and approval decision, in order.
Durability and audit are one mechanism, by design (the write-ahead-log pattern:
one log, crash-recovery + history).

## Shared state: the filesystem role

A session is an *execution* scope, not a *data* scope. Data that outlives and
crosses processes/sessions does **not** belong in the journal (which is per-process) and is
**not** shared by widening session scope — that is not what operating systems do.
Unix keeps cross-session durable data in a *separate* abstraction, the
**filesystem**, reached from any session through mediated (permissioned) access:
your login session dies, `$HOME` persists, and tomorrow's shell reads the history
yesterday's wrote. That is literally cross-session agent memory.

The scope hierarchy (`tenant → session → process → revision`) says where shared data
lives by *level*:

- **process** — process memory; the journal; reconstructed by replay.
- **session** — conversation state (dialogue context, process sequence).
- **tenant** — the `$HOME` role: cross-session memory (preferences, learned
  facts, standing context). **This is the shared-data home**; without it,
  "data shared between sessions" has no principled place.

Two kernel laws dictate the *form* the tenant store must take — it is not a
special case, it is a driver:

1. **Determinism (law #2)** forbids ambient reads of shared mutable state (a
   concurrent mutation from another session would diverge replay). So the store
   sits **behind a journaled syscall** (`core.memory`'s `get`/`put` operations, or
   file-flavoured `fs.*`): the *read result* is committed to the journal, and
   replay re-reads the recorded value regardless of the store's current
   contents. This is identical to how the ultimate shared mutable store — the
   internet — is already handled behind `core.internet`. Cross-session memory is
   *a shared mutable device behind a driver*.
2. **No ambient authority (law #1)** makes it a **capability**: tenant-scoped,
   attenuated per manifest to explicit mounts (an agent sees only the process,
   session, and named shared spaces its grant opens — the mount list does
   directory permissions for free), and governable (`require_approval` may gate
   writes to standing memory). Cross-*tenant* sharing is forbidden by
   construction — multi-tenant isolation outranks the metaphor (no Unix
   world-readable equivalent).

Security payoff: **memory poisoning** (planting an instruction that the agent
"remembers" and later treats as trusted) becomes visible and policeable — a
write from a process whose inputs were `untrusted_web`-tainted stores that label
with the value (M4 provenance), so a later read surfaces it as untrusted content
rather than laundered truth. Most agent stacks bolt memory on as an ambient RAG
lookup outside all governance; here it is a journaled, labelled, attenuated
syscall.

Concurrency: like a filesystem shared across sessions, concurrent writers need
coordination. v1 may be last-writer-wins on the `put` operation; compare-and-set is the
upgrade. (See PLAN.md "Tenant memory".)

## Coherence under growth: versioned replay

This is the **known hard problem** for any journal-replay system, capcompute
included. Name it now, because it is the fault line where the clean model meets
software evolution.

**The problem.** A process is reconstructed by replaying its journal against its
program. But programs change. When *program v2* meets a journal written by *program
v1*, replay must still produce a consistent process — and that is not free:

- Invariant #2 (determinism) means the replayed syscall sequence must match what
  the new code produces. Adding/removing pure logic (e.g. log lines) is safe;
  changing the *number, order, or arguments* of syscalls diverges from the journal
  and replay fails.
- This is unsolved in the general case. Golem — further along than us — only
  guarantees compatible changes and is still actively working state migration and
  hot-update recovery (see golemcloud/golem issue #534). Temporal exposes the same
  constraint as "non-deterministic workflow changes" and pushes users to versioned
  branching. **There is no free lunch here; there is only a chosen discipline.**

**What we owe the design (decide before it bites):**

1. **Version the program in the journal.** Every journal records the program
   version that produced it. Replay knows which code it is replaying against.
2. **Define the compatibility contract.** State plainly which changes are
   replay-safe (pure logic, added tail capabilities) and which are breaking
   (reordering/removing/retyping syscalls). This is the guest-author's law, the
   same way the ABI is the driver-author's law.
3. **Provide an escape hatch for breaking changes.** Options, cheapest first:
   - *drain* — finish all in-flight processes on v1 before deploying v2 (simplest;
     often enough for short-lived processes);
   - *pinned replay* — replay each process against the exact version that wrote its
     journal, and only run *new* processes on the new version (Golem's default
     posture);
   - *migration* — a guest-provided function that transforms v1 journal/state into
     v2 (most powerful, most complex; defer until required).
4. **Snapshot to bound replay cost.** As journals grow, replay-from-zero gets
   expensive. A periodic committed snapshot (checkpoint) of process state caps
   replay to "since last snapshot" — the classic single-level-store move, and it
   also gives migration a clean seam.

## Non-goals (resist gold-plating)

The metaphor is a map, not a checklist. Implement an OS concept only when a real
requirement forces it. Not planned unless needed: preemptive scheduling, virtual
memory/paging, a POSIX filesystem, signals beyond cancel, and async multi-process
IPC. A uniprocessing, cooperatively-scheduled OS is still an OS.
