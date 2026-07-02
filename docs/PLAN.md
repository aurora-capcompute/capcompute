# Aurora execution plan

The single sequenced worklist. Consolidates `ROADMAP.md` (items that flow from
the OS model), `RESEARCH.md` (durable-execution / capability findings), and
`CHALLENGE.md` (the adversarial audit), plus items raised in design discussion
that were never written down (the `cred` rename, the spawn decorator spec).

Read the other docs for *why*; this doc is *what, in what order, and done-when*.

## Status legend
`DONE` shipped · `NEXT` cleared to start · `BLOCKED(x)` waits on x ·
`SPEC` design-only until a dependency forces it · `DEFER` intentionally not now.

## Already shipped (context)
Kernel/OS rename; ABI v2 (`abi` field, `sys.Errno`, `sys.begin`/`sys.commit`);
ambient lockdown (`ambient.go`, `ErrAmbientAuthority`); journal program-
versioning (`journaled.Header`, `ReplayIncompatibleError`); `sys.Attenuate`;
kernel-law tests (laws 1–2). Consumers migrated; k8s-agent stays pre-rename
pinned (blocked on out-of-scope `aurora-capcompute`/`aurora-stores`).

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
- **M2.2 Aggregate per-cred accounting** — `BLOCKED(M5.1 scheduler seam)`
  Bytes / syscalls-per-sec / concurrent-process caps per cred, enforced in the
  scheduler. Deferred to where the seam exists.
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
  hierarchy** `tenant → thread (session/SID) → run (PID) → revision` (+ parent/
  group PGID once spawn lands) plus `position`, `kind`
  {intent|completion|savepoint|…}, `prev_hash`, journaled timestamp — i.e. the
  store's index keys, aligned 1:1 with OTel trace/span/parent so the exporter
  is a column mapping. Single source of truth: a datum is an envelope column
  *or* in the payload, never both. Goal: the store schema stops changing when
  new record types appear; "log within thread" and "log within run" are index
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
- **M4.3 Declassification as a governed operation** — staged: `FlowMonitor.Declassify` (the mechanism) is `DONE`; the approval-composed syscall surface for it is `NEXT`
  Moving a value across a label boundary is an explicit operation that composes
  with `require_approval` (a human authorizes the crossing). DIFC declassify,
  gated by the approval flow you already have.
- **M4.4 Dual-LLM / control-flow-integrity brain** — `SPEC` then `BLOCKED(brain work)`
  Trusted plan from the user prompt; quarantined processing of untrusted tool
  outputs with no tool access. The deepest robustness layer; a brain-architecture
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
- **M5.1 Scheduler seam** — `SPEC` (CHALLENGE F, ROADMAP #15). Deliberately not built yet: sync-first spawn needed no scheduler (the child borrows the parent's quantum), and the app-owned scheduler this widens lives in the out-of-scope runtime — building the seam without its consumer would be speculative. Revisit when async spawn or aggregate quotas (M2.2) become real.
  Widen the app-owned scheduler into an interface with priority + admission
  hooks + virtual-actor activation/deactivation (bound resident memory; Orleans/
  Golem suspend-to-zero). Also the home for M2.2 aggregate quotas. Needed by
  spawn (children must be scheduled).
  DoD: a default fair-share scheduler; idle processes evict and reactivate.
- **M5.2 `sys.spawn` decorator (sync-first)** — `DONE` (`spawn.go`; child cred derives from the spawn's idempotency key — stronger than the sketched spawn_seq — and child execution goes through the `ChildRunner` seam, `KernelChildRunner` kernel-backed) (ROADMAP #5)
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
- **M5.3 IPC + supervision** — `SPEC` then `BLOCKED(M5.2)` (CHALLENGE I)
  Capability-passing message send/receive, each journaled; deterministic
  interleaving via a per-receiver ordered input log; supervision as process
  metadata (OTP strategies: one-for-one/one-for-all/rest-for-one, max-restart-
  intensity, orphan handling). Spec now in `ARCHITECTURE.md`; build when spawn
  forces multiprocess.
- **M5.4 Unforgeable capability references** — `DEFER` (CHALLENGE J)
  Only if guest-to-guest capability delegation (via IPC) is needed. Until then
  document the model as authorized-by-cred, not by unforgeable token.

## M6 — Tenant memory: the filesystem role  (ARCHITECTURE "Shared state")

Goal: a principled home for data shared *across threads* — the `$HOME` role.
Sessions are execution scope, not data scope; cross-thread memory belongs to the
**tenant** level, reached as a capability, never by widening thread scope. This
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
  DoD: two threads of one tenant share state via get/put; a replay re-reads the
  journaled value; an agent attenuated to a subtree cannot read outside it;
  cross-tenant access denied.
- **M6.2 Provenance-labelled memory (memory-poisoning defense)** — `NEXT` (M4.1 and M6.1 both landed; store the value's labels on put, restamp on get)
  `memory.put` stores the value's labels (M4 provenance); `memory.get` surfaces
  them, so a value written from an `untrusted_web`-tainted run resurfaces in a
  later thread *as untrusted*, not as laundered truth. This is the differentiator
  vs ambient-RAG memory (which launders provenance). Compose with M4.2 flow
  policy (untrusted memory may not flow into privileged capabilities without
  declassification).
  DoD: a write's taint is stored and re-surfaced on read in a later thread;
  flow policy blocks tainted memory reaching a protected capability.
- **M6.3 Write concurrency: CAS** — `DEFER`
  v1 is last-writer-wins on `memory.put`; add compare-and-set (version token in
  the value) when concurrent writers across a tenant's threads become real.

---

## ABI v3 — protobuf envelope  — `BLOCKED(TinyGo round-trip verification)`  (CHALLENGE E, decided; M3.1/M4.1 prerequisites landed)

Decision recorded in CHALLENGE.md E: keep the uniform envelope (mediation
uniformity — the seccomp/strace argument; wazero has no component model, so
WIT would force a runtime switch), migrate the *encoding* to protobuf once the
record shape settles.

- Motivations in order: schema-evolution discipline for long-lived journals
  (serves versioned replay); protovalidate/CEL as a stronger monitor policy
  substrate; per-field sensitivity/provenance annotations via custom field
  options (feeds M4); typed codegen. **Not** performance (in-process copy at
  LLM-turn cadence).
- Caveats: TinyGo needs codegen-only protobuf (vtprotobuf-style; `prost` for
  Rust) — verify a TinyGo round-trip first; keep a `protojson` rendering path
  so journals/audit stay human-readable.
- Lands as a clean cut: `abi: 3` in the envelope; guests and host migrate
  together (no backwards compatibility, per prototype policy).
  DoD: proto envelope round-trips host↔both brains; journal records in proto
  with protojson display; ABI v2 rejected with `bad_abi`.

## Cross-cutting (do alongside, not as a milestone)

- **Journal lifecycle** — unblocked (M3.1 landed), `DEFER` until journals carry real volume (CHALLENGE G, supersedes ROADMAP #7):
  snapshot + compaction + retention once journals carry real intent/completion
  volume; verifiable archived segments via the hash-chain.
- **Hash-chained journal** — `DONE` with M3.1 (ROADMAP #3): `prev_hash` per record,
  `journaled.Verify` walks structure + chain.
- **Journal → OpenTelemetry** — `DONE` (`otelexport/`) (CHALLENGE H, ROADMAP #17):
  run=trace root, intent=span, completion folds in, open intent=error span.
- **Kernel-law tests (laws 3–5)** — law 3 `DONE` (the `sim/` crash matrix asserts
  journal-before-execute/observe); law 4's approval-gate assertion still lives
  with the runtime's approval machinery (out of scope here).

## k8s-agent / runtime crossover — `BLOCKED(out-of-scope modules)`
`aurora-capcompute` + `aurora-stores` must migrate to the renamed ABI before
aurora-k8s-agent can adopt any of the above. When they do: task `Kind` field
(RESEARCH 2), attenuation-at-grant + revocation epochs (RESEARCH 4), sources-
as-inbound-drivers (ROADMAP #8), Manifest CRD OS-convention naming. Tracked in
`aurora-k8s-agent/AGENTS.md`.

---

## Recommended starting point
The original four cheap items, M3 (intent records + compensation + DST), M4.1/2
(provenance + flow policy), M5.2 (spawn), M6.1 (tenant memory), and the
hash-chain/OTel cross-cutters are all `DONE`. The live frontier, in value
order: **M6.2** (provenance-labelled memory — the memory-poisoning defense,
now fully unblocked), **M4.3**'s declassification syscall surface, **ABI v3**
once a TinyGo/prost protobuf round-trip is verified, and **M5.1/M5.3** when a
scheduler consumer or async multiprocess need appears. The k8s-agent crossover
stays blocked on the out-of-scope module migrations.
