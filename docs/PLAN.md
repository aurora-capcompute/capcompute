# Aurora execution plan

The single sequenced worklist. Consolidates `ROADMAP.md` (items that flow from
the OS model), `RESEARCH.md` (durable-execution / capability findings), and
`CHALLENGE.md` (the adversarial audit), plus items raised in design discussion
that were never written down (the `cred` rename, the spawn decorator spec).

Read the other docs for *why*; this doc is *what, in what order, and done-when*.

> File references in the milestones below predate the 2026-07-19 charter
> passes: the monitor (validate/provenance/stack), replay, journal, scheduler,
> and DST harness now live in aurora-capcompute (`monitor/`, `replay/`,
> `journaled/`, `internal/sched/`, `sim/`).

## Status legend
`DONE` shipped · `NEXT` cleared to start · `BLOCKED(x)` waits on x ·
`SPEC` design-only until a dependency forces it · `DEFER` intentionally not now.

## Already shipped (context)
Kernel/OS rename; ABI v2 (`abi` field, `sys.Errno`, `sys.begin`/`sys.commit`);
ambient lockdown (`ambient.go`, `ErrAmbientAuthority`); journal program-
versioning (`journaled.Header`, `ReplayIncompatibleError`); `sys.Attenuate`;
kernel-law tests (laws 1–2). Consumers migrated (the full runtime migration
shipped later — see the crossover section and the distribution epoch).

---

## Guiding sequence

Value × cost, honest about dependencies. Five milestones. M1–M2 are cheap,
high-value, fully inside capcompute. M3 is the durability core. M4 is the
security frontier. M5 is the multiprocess future.

```
M1 Harden the monitor      → M2 Resource control      → M3 Durable effects
   (cheap security)            (cheap DoS close)          (intent→compensation, DST)
                                                              │
                                                              ▼
M4 Data-flow security  ←──────────────────────────────  M5 Multiprocess
   (IFC/provenance — frontier)                              (cred rename → spawn → IPC)
```

`cred` rename (M5.0) is a prerequisite for spawn and should land early and
mechanically even though the rest of M5 is later — it touches every dispatcher
signature and is cheapest before more code piles on.

---

## M1 — Harden the reference monitor  (CHALLENGE C, E-part-1)

Goal: the monitor validates *every* access. Closes the confused-deputy hole.
Fully inside capcompute; no consumer break beyond a new decorator they opt into.

- **M1.1 Grant-set + schema validation decorator** — `DONE` (`validate.go`)
  A kernel-provided `sys.Dispatcher` decorator that, before delegating:
  (1) checks the cred's granted capability set contains `syscall.Name` →
  `FailCode(ErrnoDenied)`; (2) validates `syscall.Args` against the capability's
  `InputSchema` → `FailCode(ErrnoInvalidArgs)`.
  Files: new `capcompute/validate.go` (root pkg — needs the grant source), test
  `validate_test.go`.
  DoD: unknown/ungranted name denied; malformed args rejected pre-dispatch;
  valid call passes through unchanged; documented as the "complete mediation"
  law in `ARCHITECTURE.md`.
  Design note: the grant set comes from the cred/manifest; decorator takes a
  `func(cred K) []sys.Capability` (or a `Capabilities()`-style source) so the
  app supplies where grants live.

## M2 — Resource control  (CHALLENGE B)

Goal: no single process or tenant can exhaust the host. Two cheap wazero-native
steps now; aggregate quotas ride the M5 scheduler seam.

- **M2.1 Per-process memory cap + resume deadline** — `DONE` (`Config.MaxMemoryPages`/`ResumeTimeout`)
  Kernel sets a wazero memory-page limit on the instance and an optional
  per-`Resume` wall-clock deadline (derives a child context from the resume ctx;
  you already cancel on ctx). Config fields `MaxMemoryPages`, `ResumeTimeout`.
  Files: `kernel.go` (`guestModuleConfig`/`RuntimeConfig` + `Resume`),
  `ambient.go` if the limit belongs with source config; tests: OOM guest traps,
  infinite-loop guest is killed by deadline (extend existing "infinite" mode).
  DoD: a guest allocating past the cap traps as `ResumeFailed`; a guest past the
  deadline returns `ResumeStopped`; defaults are unlimited (opt-in).
- **M2.2 Aggregate per-cred accounting** — `DONE` (`sched.Quota` per-owner
  concurrency caps as backpressure in the scheduler — now in
  `aurora-capcompute/internal/sched`. `capcompute.Throttle` token-bucket rate
  limiting was built on the delays-never-denies law — a wall-clock refusal
  would break guest determinism — then removed unconsumed. Aggregate bytes =
  per-owner residency × `MaxMemoryPages`.)
- **M2.3 Deterministic CPU fuel** — `DEFER` (frontier)
  True instruction-budget metering would make CPU part of journaled state, but
  wazero has no fuel; needs a shim or wasmtime. Revisit only if repro CPU limits
  become a requirement.

## M3 — Durable effects: the write-ahead core  (RESEARCH 8–9)

Goal: nothing changes the world without a trace, and multi-step effects can be
unwound. This is the durability heart and the biggest audit-story win. DST
(M3.3) is the test home for the crash-timing correctness of M3.1–M3.2.

- **M3.1 Intent/completion journal records** — `DONE` (two-record tape, `OpenIntentPolicy`, hash chain) (ROADMAP #9)
  Tape appends an **intent** record before dispatch, a **completion** after.
  Replay meeting an open intent at the tail = typed *indeterminate* condition
  (new error, not divergence) with per-capability policy (fail-for-review on
  mutations, retry on reads). Disambiguate: open intent + pending task = waiting.
  Intent identity `(PID, position, call-hash)` doubles as an idempotency key
  passed to drivers.
  Files: `sys/replay/tape/journaled/tape.go` (two-record protocol + open-intent
  detection), `sys/replay/replay.go` (record intent before delegating), new
  `IndeterminateError`; the `Journal` interface gains intent/completion append.
  DoD: crash-before-completion leaves a detectable open intent; replay surfaces
  it per policy; splits invariant #3 into journal-before-observe (held) +
  **journal-before-execute** (new) in `ARCHITECTURE.md`.
  **Record-schema principle (CHALLENGE K), applied here since M3.1 reshapes the
  record anyway):** one record = uniform **envelope** + **opaque payload** (the
  syscall envelope, same shape as the ABI). Envelope = the fixed **scope
  hierarchy** `tenant → session (SID) → run (PID) → revision` (+ parent/
  group PGID once spawn lands) plus `position`, `kind`
  {intent|completion|savepoint|…}, `prev_hash`, journaled timestamp — i.e. the
  store's index keys, aligned 1:1 with OTel trace/span/parent so the exporter
  is a column mapping. Single source of truth: a datum is an envelope column
  *or* in the payload, never both. Goal: the store schema stops changing when
  new record types appear; "log within session" and "log within run" are index
  scans. Downstream SQLite/`task.Record` adopt the same contract on the runtime
  migration (blocked).
- **M3.2 Compensation metadata + saga unwinding** — `DONE` (`Capability.Compensation`, `saga.go`) (ROADMAP #10)
  Add declared `Compensation` to `sys.Capability` (inverse syscall name, or
  explicit cannot-compensate). Kernel-level unwind: on scope abort, walk the
  journal's completed effects in reverse and dispatch compensations — each
  journaled, each composable with `require_approval`; human escalation (with the
  journal) is the terminal compensator. Reframe `sys.begin`/`sys.commit` in docs
  as **redo scopes**, and flag brackets over non-idempotent un-keyed effects.
  Files: `sys/dispatcher.go` (`Capability.Compensation`), new
  `capcompute/saga.go` (unwind walk), docs.
  DoD: an aborted scope compensates completed effects newest-first; a
  cannot-compensate effect escalates; unwinding is itself in the journal.
- **M3.3 Deterministic simulation testing harness** — `DONE` (`sim/`) (ROADMAP #14, CHALLENGE D)
  A harness driving the kernel with a mock `ProcessTable` and a fault-injecting
  dispatcher; script a crash at *every* journal position; assert M3.1/M3.2
  invariants across the matrix (replay convergence, effect idempotency, no
  orphaned intents, unwind correctness).
  Files: new `capcompute/sim/` test package.
  DoD: the fault matrix runs in CI and passes; a deliberately introduced
  order-bug is caught by it.

## M4 — Data-flow security: information flow control  (CHALLENGE A)

Goal: track *where values come from and may flow*, not just what may be called.
The frontier bet; the biggest differentiator (CaMeL applied as a kernel
primitive). Staged so value lands before the deepest part.

- **M4.1 Provenance labels on results** — `DONE` (`Capability.Labels`, `Labeler`) (ROADMAP #11a)
  `SyscallResult` carries a taint/label set (deriving capability + declared
  source class, e.g. `untrusted_web`, `secret`). Dispatcher stamps; journal
  records (provenance becomes part of the audit trail for free).
  Files: `sys/syscall.go` (`SyscallResult.Labels`), dispatcher stamping, journal
  record extension.
  DoD: results carry labels; labels are journaled and visible in the trace.
- **M4.2 Flow policy at the reference monitor** — `DONE` (`Capability.Forbid`, `FlowMonitor`) (ROADMAP #11b)
  The monitor rejects a syscall whose args carry labels the target capability
  forbids (`k8s.delete` may not take `untrusted_web` data) → `ErrnoDenied`.
  Label propagation lives at the dispatcher boundary (the guest is opaque), so
  no guest instrumentation needed. This is CaMeL's enforcement, minus a custom
  interpreter.
  DoD: a tainted-arg call to a protected capability is refused; policy is
  per-capability metadata.
- **M4.3 Declassification as a governed operation** — `DONE` (`sys.declassify`: reason required, approval mandatory, crossing journaled and replayed without re-asking; `Declassifier` below replay + `FlowMonitor` removal above)
  Moving a value across a label boundary is an explicit operation that composes
  with `require_approval` (a human authorizes the crossing). DIFC declassify,
  gated by the approval flow you already have.
- **M4.4 Dual-LLM / control-flow-integrity program** — `DONE` (the camel
  program in aurora-brains: quarantined planner, `$N` routing — ROADMAP #24)
  Trusted plan from the user prompt; quarantined processing of untrusted tool
  outputs with no tool access. The deepest robustness layer; a program-architecture
  change in aurora-brains, not a kernel change. Spec in `ARCHITECTURE.md`; build
  after M4.1–M4.3 prove the labelling substrate.

## M5 — The multiprocess future  (spawn, IPC, supervision)

Goal: agents create and coordinate agents, governed and replayable. `cred`
rename first (mechanical, unblocks everything), then spawn, then IPC.

- **M5.0 Rename `guestData` → `cred`** — `DONE` (discussion item)
  `Dispatch(ctx, cred K, …)`, `Process.GuestData` → `Cred`, `ProcessSpec.UserData`
  → `Cred`. Document the syscall triad (cred = who / syscall = what / auth =
  granted) and the driver-stratification rule (leaf drivers ignore cred; only
  policy decorators consume it) in `ARCHITECTURE.md`. Mechanical across
  capcompute + aurora-dispatchers.
  DoD: renamed, builds/tests green in both repos, glossary + triad documented.
  Note: false-friend fix — "guestData" reads as guest-owned; it is host-side
  credentials.
- **M5.1 Scheduler seam** — `DONE` (`sched` package: Activate/Resume/Deactivate
  seams; default fair-share scheduler — strict
  priority bands, owner round-robin, per-owner quotas, virtual-actor residency
  with LRU eviction and reactivation-by-replay; race-tested). The runtime
  adopted it, and the package then **moved to its consumer**
  (`aurora-capcompute/internal/sched`) — scheduling is runtime policy, not a
  kernel primitive (the charter); the unused `KernelResume` binding was
  dropped in the move.
  Widen the app-owned scheduler into an interface with priority + admission
  hooks + virtual-actor activation/deactivation (bound resident memory; Orleans/
  Golem suspend-to-zero). Also the home for M2.2 aggregate quotas. Needed by
  spawn (children must be scheduled).
  DoD: a default fair-share scheduler; idle processes evict and reactivate.
- **M5.2 `sys.spawn` decorator (sync-first)** — `DONE`, then **removed
  unconsumed** (the IPC razor): built as `spawn.go` — attenuation,
  idempotency-key child identity (stronger than the sketched spawn_seq),
  transitive yield, the `ChildRunner` seam — but the runtime serves
  `sys.spawn` with its own router (manifest-carrying grants), so the kernel
  decorator had no caller. The name stays reserved; the decorator returns by
  revert only if kernel-composed children are ever needed. (ROADMAP #5)
  Kernel-provided decorator intercepting reserved `sys.SyscallSpawn`; delegates
  else. `spawn(program, input, capabilities)` with: capabilities enforced ⊑
  parent via `sys.Attenuate`; deterministic child PID `f(parentPID, spawnSeq,
  program)`; child gets its **own journal** keyed by child PID; child result
  committed into the parent's tape (replay re-finds, does not re-spawn); parent
  yields while child runs (child-workflow pattern). Host creation
  (`CreateProcess`) stays a direct kernel API — the init/PID-0 exception.
  App supplies a `DeriveCred(parent K, seq int, program string) K` seam.
  Files: new `capcompute/spawn.go`, `ARCHITECTURE.md` spawn section (transitive
  yield: parent's spawn yields when the child yields; resume re-enters the
  child's journal).
  DoD: sync child completes and commits to parent journal; replay does not
  re-spawn; capability escalation refused; child-yield propagates to parent and
  resumes correctly.
- **M5.3 Supervision** — `DONE`, then **removed unconsumed** (built as
  `sched/supervisor.go`: one-for-one/one-for-all/rest-for-one via
  Scheduler.Stop + resubmit, restart intensity, escalation; razored with the
  spawn decorator — the runtime owns child lifecycle through its delegation
  tasks) (CHALLENGE I)
  Supervision is process metadata (OTP strategies: one-for-one/one-for-all/
  rest-for-one, max-restart-intensity, orphan handling); spec in
  `ARCHITECTURE.md`. IPC (sys.send/sys.recv, a `Messenger` decorator) was
  prototyped and then **removed** — it was never wired into the runtime stack,
  so it was dead weight. Cross-process/-session data sharing, if it is ever
  needed, becomes an explicit granted channel, not ambient message-passing.
- **M5.4 Unforgeable capability references** — `DEFER` (CHALLENGE J)
  Only if guest-to-guest capability delegation (via IPC) is needed. Until then
  document the model as authorized-by-cred, not by unforgeable token.

## M6 — Tenant memory: the filesystem role  (ARCHITECTURE "Shared state")

Goal: a principled home for data shared *across sessions* — the `$HOME` role.
Sessions are execution scope, not data scope; cross-session memory belongs to the
**tenant** level, reached as a capability, never by widening session scope. This
is a **driver-layer** feature, independent of the M1–M5 queue.

- **M6.1 Tenant-scoped store capability** — `DONE` (`aurora-dispatchers/memory`, `core.memory` registration: tenant scoping, subtree chroot, approval-gated writes, replay re-read test)
  A `memory.get` / `memory.put` capability (file-flavoured `fs.*` also fine),
  implemented as a dispatcher/driver in `aurora-dispatchers` over a
  tenant-scoped KV store. The two kernel laws fix its form:
  (1) **determinism** — it goes *through* the journaled syscall path, so a read
  result is committed and replay re-reads the recorded value regardless of later
  mutations (identical to `internet.read`, the existing shared-mutable device);
  (2) **no ambient authority** — tenant-scoped, attenuable per manifest (an agent
  is granted only a subtree — the grant tree = directory permissions),
  `require_approval`-gatable on writes. Cross-tenant sharing forbidden by default.
  Files: new `aurora-dispatchers/memory/` (driver) + a store interface the app
  supplies; capability schema in `registry`.
  DoD: two sessions of one tenant share state via get/put; a replay re-reads the
  journaled value; an agent attenuated to a subtree cannot read outside it;
  cross-tenant access denied.
- **M6.2 Provenance-labelled memory (memory-poisoning defense)** — `DONE` (`memory.put` stores the writer's taint via `sys.Taint`, `memory.get` restamps it; cross-session poison test drives the full stack)
  `memory.put` stores the value's labels (M4 provenance); `memory.get` surfaces
  them, so a value written from an `untrusted_web`-tainted run resurfaces in a
  later session *as untrusted*, not as laundered truth. This is the differentiator
  vs ambient-RAG memory (which launders provenance). Compose with M4.2 flow
  policy (untrusted memory may not flow into privileged capabilities without
  declassification).
  DoD: a write's taint is stored and re-surfaced on read in a later session;
  flow policy blocks tainted memory reaching a protected capability.
- **M6.3 Write concurrency: CAS** — `DEFER`
  v1 is last-writer-wins on `memory.put`; add compare-and-set (version token in
  the value) when concurrent writers across a tenant's sessions become real.

---

## ABI v3 — protobuf envelope — `DONE` (CHALLENGE E)

Decision recorded in CHALLENGE.md E: keep the uniform envelope (mediation
uniformity — the seccomp/strace argument; wazero has no component model, so
WIT would force a runtime switch), migrate the *encoding* to protobuf once the
record shape settles.

- Shipped as a clean cut: `abi: 3`; host and both guests migrated together;
  the pre-v3 JSON wire is just garbage to the decoder (`invalid_args`).
- **Deviation from the sketch, deliberate:** instead of vtprotobuf/prost
  codegen in guests, the envelope codec is hand-rolled proto3 wire format
  (`sys/wire`, ~200 dependency-free lines; mirrored in `aurora-brains sdk/src/wire.rs`).
  This dissolves the TinyGo gate rather than passing it — no `protoreflect`
  in any guest — and honors minimal-TCB. Interop is pinned three ways:
  both-direction round-trips against protoc-generated reference code
  (`sys/wire/internal/refpb`, regenerate with `protoc --go_out`), golden byte
  fixtures shared verbatim between Go and Rust tests, and unknown-field
  skipping (the schema-evolution contract). `envelope.proto` stays the
  source of truth, so protovalidate/CEL and field annotations can adopt real
  codegen later without a wire change.
- **Journal records stay canonical JSON** — the wire and the store encoding
  are separate concerns, and readable journals were the point of the
  protojson caveat; store-side proto adoption rides the (blocked) runtime
  migration if it ever pays.
- Verified here: host round-trip + protoc interop (Go tests), Rust program
  `cargo test` + release build for wasm32-wasip1. The Go program and the
  integration guest share the host-tested codec and typecheck for wasip1;
  their tinygo compile runs in CI (no tinygo in this container).

## Cross-cutting (do alongside, not as a milestone)

- **Journal lifecycle** — unblocked (M3.1 landed), `DEFER` until journals carry real volume (CHALLENGE G, supersedes ROADMAP #7):
  snapshot + compaction + retention once journals carry real intent/completion
  volume; verifiable archived segments via the hash-chain.
- **Hash-chained journal** — `DONE` with M3.1 (ROADMAP #3): `prev_hash` per record,
  `journaled.Verify` walks structure + chain.
- **Journal → OpenTelemetry** — built (`otelexport/`), then **removed
  unconsumed** (CHALLENGE H, ROADMAP #17); the mapping (run=trace root,
  intent=span, completion folds in, open intent=error span) returns as
  aurora-dist ops (ROADMAP #29).
- **Kernel-law tests (laws 3–5)** — law 3 `DONE` (the `sim/` crash matrix asserts
  journal-before-execute/observe); law 4's approval-gate assertion still lives
  with the runtime's approval machinery (out of scope here).

## k8s-agent / runtime crossover — `DONE` (runtime migration shipped)
`aurora-capcompute` runs on the kernel head: Stack-assembled chains,
hash-chained intent/completion journal over the event log (per-revision
headers), open-intent durable tasks injecting the stored resolution as the
dispatch Authorization, sys.begin/commit savepoint forking, and root runs as
`sched.Scheduler` quanta (children ride the parent's quantum — the sync-spawn
posture). `aurora-stores` ships ProcessTable, event log, leases, and a
durable `journaled.Journal` with a Verify audit path. aurora-k8s-agent
resolves the whole graph at real pins, green. Remaining crossover follow-ups
tracked in `aurora-k8s-agent/AGENTS.md`: the `-llm`/`-k8s`/`-helm` driver
modules still await their sys migration (unplugged behind a documented seam),
plus `core.memory`/IFC fields on the Manifest CRD; task `Kind` (RESEARCH 2),
attenuation-at-grant + revocation epochs (RESEARCH 4), and sources-as-inbound-
drivers (ROADMAP #8) stay open design items.

---

## The distribution epoch — target topology (decided 2026-07-03)

The ecosystem contracts to three library repos and will grow two product
repos; everything else is deprecated. Read this section as the successor to
the milestone queue above: M1–M6 built the OS, this builds what ships it.

**Surviving cores:**
- `capcompute` — the kernel. Unchanged role.
- `aurora-capcompute` — the runtime. Unchanged role; D0 vocabulary below.
- `aurora-dispatchers` — the driver library (domain driver modules —
  `-llm`/`-k8s`/`-helm` — migrate to `sys` and re-plug when needed).

**New products (to be created):**
- `aurora-dist` — the distribution: one binary compiling the runtime with a
  chosen driver set and store implementations (absorbing `aurora-stores`'
  role), exposing the runtime over **one HTTP API** — the single way in,
  versioned `/v1` from day one. Owns the runtime-adjacent services that must
  not live in terminals: **timer firing** (durable-wait resolution — today it
  wrongly lives in a channel bridge), the **program directory** (re-scanned
  into the runtime by polling so the in-memory set tracks the filesystem),
  and a **static capability ceiling** (`CreateRun` refuses
  manifests granting beyond the deployment's configured maximum —
  `sys.Attenuate` at the door; defense in depth against a compromised policy
  layer).
- `aurora-cli` — the first terminal: a CLI binding directly to `aurora-dist`
  (trusted local single-principal use; no policy layer between). Building it
  validates the API's completeness before any networked connector exists.

**The policy layer** (when multi-principal): a separate authorization
service in front of `aurora-dist` — the microkernel move, mechanism in the
distro / policy in a server. It owns: the **manifest registry**
(named/versioned manifests; the runtime itself stores manifests only
per-run, journaled — there is deliberately no manifest entity in the core),
**principal authentication**, **per-credential capability ceilings**
(attenuation-at-grant — RESEARCH 4 lands here), the **session directory**
(session ↔ principals/binding/channel address: the session-level access
control the distro deliberately lacks; also what makes sharing and
cross-channel identity expressible — a directory, never a mirror of session
state), **HITL resolution authority** (who may resolve which task — today
resolution is bearer-token-only), and the **data-plane proxy** for
terminals (full proxy first: the distro then has exactly one client and
zero principal auth). Connectors (Telegram etc.) become pure terminals —
transport + rendering + their own state, zero policy — attaching to
sessions through it.

**Deprecated:** `aurora-k8s-agent` (its CRD control plane may return as one
backend of the policy layer's manifest registry; its chat cores as connector
services), `aurora-stores` (implementations fold into `aurora-dist`),
`aurora-brains` (the example-program workspace; program packaging moves to
the distro pipeline — until then the runtime's integration tests build the
agent program from the sibling checkout).

**Upgrade doctrine** (why program upgrades stay a non-problem here, unlike
immortal-worker systems): the unit of replay is the bounded **process** —
sessions carry continuity as data (history, the log), never as live guest
state. A process is immutably bound to the (name, identity) it was created
from — where identity is the digest of the wasm **and** its interface manifest
together, so both the code and the declared contract are fixed. The journal
header refuses drift on replay, and the runtime refuses resume *and* restart
under a changed identity, keeping the process a legitimate audit target. So
upgrades are **drain-and-deprecate**: new processes bind the new program;
parked processes drain within TaskTTL; decommission once no non-terminal
process references the identity, keeping exact old artifacts (content-addressed)
until then. ABI bumps remain fleet-wide drain events by design. Dispatcher
upgrades follow the same story once D0.2 lands.

### D0 — executable now, inside the three surviving repos
- **D0.1 Vocabulary cut: `thread` → `session`, the cognition unit → `program`.**
  Session is the OS-correct term for the level that groups processes and is
  what a controlling terminal attaches to; "thread" inverted the metaphor
  (OS threads live *inside* processes). Program finishes a rename the kernel
  (`Header.Program`, `sys.spawn`) and assembly already made. API, internals,
  and wire (`session_id`, `program`, `ProgramDigest`, `ses_` id prefix, task
  scopes, event payload fields) — a clean cut; old event logs do not fold.
  The guest ABI names (`sys.input`/`sys.output`/`sys.log`) are the
  program SDK's contract, not scope vocabulary — unchanged.
- **D0.2 Restore quarantine.** `restore()` must not refuse to boot because a
  historical run's manifest no longer validates against the compiled driver
  set (today, decommissioning a dispatcher bricks boot). Quarantine instead:
  warn, restore verbatim; an execution attempt fails with the provider's
  error. This makes dispatcher upgrades drain-and-deprecate too.
- **D0.3 Doc alignment.** The envelope scope hierarchy reads
  `tenant → session → run → revision` with no gloss; shared-state prose
  speaks sessions.

### D0.4 — the process vocabulary (decided 2026-07-03, cut with D1)
`run` → `process`, completing the OS metaphor D0.1 started: a session groups
processes the way a terminal session does, and the thing a session groups was
still called a "run". The scope hierarchy reads `tenant → session → process →
revision`; a process pins a program digest; a revision is one incarnation of
a process (the kernel keys instances by `pid@revision`, so a forked retry can
never resume a stale instance). Kernel surface: `Stack.ForProcess`,
`Taints.ForgetProcess`, `journaled.Header.Process`, `ProcessUnwoundError`.
Runtime surface and wire: `CreateProcess`/`GetProcess`, `process_id`, `proc_`
id prefix, `process.state` events. Like D0.1 this is a clean cut — old event
logs and journal headers do not fold. The guest ABI is unchanged (`agent.*`,
`sys.*`, entrypoint `run` — a wasm export name, not scope vocabulary).

### D1+ — in order, as the new repos are born
- **D1 `aurora-dist`**: assemble runtime + drivers + stores; the API surface
  (port of the k8s-agent webapi, `/v1`); timer firing; program directory;
  capability ceiling. The `resolution_token` and `session_id`
  renames were the cautionary tales for why the API versions from birth.
- **D2 `aurora-cli`** end-to-end against the API — the completeness test for
  the read/poll surface.
- **D3 the policy layer + first real connector**, per the design above.

### D1+D2 — DONE (2026-07-03)
- **D1 `aurora-dist` shipped.** One binary: runtime + compiled-in drivers
  (builtin, internet, MCP, memory, timer, `openaillm` — the LLM driver
  migrated from `aurora-dispatchers-llm` into `aurora-dispatchers` under the
  `sys` vocabulary) + stores absorbed from `aurora-stores` (in-memory and
  SQLite: event log with `session_id`/`process_id` columns, leases, journal
  store with Verify, and the tenant-memory KV). The `/v1` HTTP API is a thin
  projection of the runtime read surface, and the dist owns timer firing (a
  ticker reconciles armed timers against runtime task state — restart-safe,
  re-arming from persisted tasks and firing elapsed ones at boot), the program
  directory (re-scanned into the runtime on a ticker, digest-diffed) and the
  capability ceiling (`sys.Attenuate` at process creation over statically
  derived grant names; open-ended MCP grants are refused under a ceiling). Verified end-to-end with the real agent program
  against a scripted OpenAI-compatible stub, including a full restart
  mid-timer-wait.
- **D2 `aurora-cli` shipped.** Pure-stdlib terminal over the dist API with
  its own wire types and a **saved kubectl-style context** (server + current
  session + current process in `$AURORA_CONFIG`), so ids chosen once are not
  retyped; `use`/`context`/`sessions` manage it, `-s`/`-p`/`-server`
  override. send/poll, journal/tasks/graph rendering, approve/deny by
  `resolution_token`. It did its job as the completeness test immediately,
  surfacing — via its restart end-to-end — a **verbatim-marshal law** the
  whole stack now obeys: durable renderings of syscalls and results must not
  HTML-escape (`SetEscapeHTML(false)` end to end), or a restored process
  re-issuing its own bytes diverges against its own journal.
- **Read-API doctrine (decided with D2): one read, rendered client-side.**
  The event log is the single source of truth, so `GET /v1/sessions/{id}`
  returns the whole session log — metadata, history, and every process with
  its full state, delegation links, journal across all revisions (each entry
  stamped with `position`+`revision`), and tasks. The call graph, the current
  journal, one revision, a task list are groupings of that one payload,
  computed in the terminal. There is no `/graph`, `/journal`, or `/tasks`
  endpoint; a single cheap `GET /v1/processes/{id}` remains for status
  polling. Mechanism (the fold) in the server, policy (the rendering) on the
  client — the same microkernel split the distro/policy-layer boundary makes.
- Newly deprecated: `aurora-dispatchers-llm` (folded into
  `aurora-dispatchers/openaillm`), `aurora-stores` (folded into
  `aurora-dist/internal/dist/store`).

## Recommended starting point
Everything designed for these repos is `DONE` — M1 through M6, ABI v3, the
runtime migration, and the distribution epoch through D2: the vocabulary cuts
(D0.1–D0.4), the `aurora-dist` distribution, and the `aurora-cli` terminal.
A 2026-07-19 charter pass then made the kernel library kernel-only: the
scheduler moved to its consumer, and the unconsumed spawn decorator,
supervisor, throttle, and OTel exporter were razored (designs kept above).
Next up is **production hardening, not D3**: the 2026-07-05 refocus (ROADMAP
"Now", items #26–31 — channel auth, server hardening, backup/restore,
metrics, LLM spend, deploy shape) comes first, and D3's substance (policy
layer, connectors) is parked with multi-tenancy and additional channels
behind their return triggers. Standing
deferrals, unchanged: **M2.3** CPU fuel waits on a wazero fuel mechanism;
**M5.4** unforgeable capability references wait on evidence that
authorized-by-cred is insufficient; **journal lifecycle** waits on real
volume; the kernel-side `Spawner` returns by revert only if kernel-composed
children are ever needed (`sys.spawn` itself is live in the runtime's router).
