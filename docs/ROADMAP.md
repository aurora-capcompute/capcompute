# Roadmap — improvements that flow from the OS model

Scored follow-ups now that the API speaks the OS vocabulary (see
`ARCHITECTURE.md` for the model and its invariants, and `RESEARCH.md` for the
prior-art review behind several items). Score = impact on
coherence/capability (H/M/L) × effort (S/M/L). The order below is the
recommended sequence; each item is deliberately small enough to land alone.

| # | Item | Impact | Effort | Status |
|---|------|--------|--------|--------|
| 0 | Ambient-surface lockdown (kernel owns guest WASI sources) | H | S | **done** (`ambient.go`, `ErrAmbientAuthority`) |
| 1 | Journal program-versioning + replay compatibility check | H | S–M | **done** (`journaled.Header`, `ReplayIncompatibleError`) |
| 2 | Kernel-law CI tests (the five invariants as tests) | H | S–M | partial — laws 1/2 tested (unit + TinyGo ambient/http modes); approval-gate test lives with the runtime |
| 3 | Hash-chained journal (tamper-evident audit) | M–H | S | **done** (`prev_hash`, `journaled.Verify`) |
| 4 | Capability attenuation helper in `sys` | M | S | **done** (`sys.Attenuate`) |
| 5 | `process.spawn` syscall (sync-first child processes) | H | M | **done** (`sys.spawn`, `spawn.go`) |
| 6 | ABI v2 bundle: version field, errnos, savepoint syscalls | M | S | **done** (`sys.ABIVersion=2`, `sys.Errno`, `sys.SyscallBegin/Commit`) |
| 7 | Snapshot/checkpoint to bound replay cost | M | L | deferred |
| 8 | Sources-as-inbound-drivers refactor (aurora-k8s-agent) | M | M | deferred |
| 9 | Intent/completion journal records (journal-before-execute) | H | M | **done** (two-record tape, idempotency keys) |
| 10 | Guest-registered compensation + abort-retry | H | M | **done** (`sys.compensate`, `sys.abort`; runtime-driven rollback) |
| 11 | Information-flow labels + provenance (CaMeL-style) | H | L | **done** — labels, flow policy, and `sys.declassify` (`provenance.go`) |
| 12 | Resource management (mem cap, resume deadline, aggregate quotas) | H | S–M | **done** (`MaxMemoryPages`/`ResumeTimeout`; `sched.Quota` + `Throttle` for aggregates) |
| 13 | Reference-monitor validation (grant-set + InputSchema) | H | S | **done** (`Validator`, `validate.go`) |
| 14 | Deterministic simulation testing harness | H | M | **done** (`sim/`, full crash matrix) |
| 15 | Scheduler seam: priority, admission, virtual-actor activation | M | M | **done** (`sched/`) |
| 16 | Journal lifecycle: snapshot + compaction + retention | M | M | removed — built, then taken out as more machinery than the problem warrants yet; streams grow unbounded until growth is a measured problem |
| 17 | Journal→OpenTelemetry exporter | M | S | **done** (`otelexport/`) |
| 18 | Exactly-once effects: drivers honor idempotency keys | H | M | **done** (memory driver activity memory; sqlite transactional) |
| 19 | Reservation / TCC as a pattern (saga isolation) | M–H | S | **done** — resolved as a *pattern* over dispatch + compensate, not a driver (see §19) |
| 20 | Approval-composable compensation (yielding inverse) | M | M | **done** (inverses dispatch through the task layer; rollback parks and resumes) |
| 21 | Deterministic rollback matrix (crash-test #10) | H | M | **done** (abort + guest-failure stories at every append position; found the lost-wakeup park and the fail-redo orphaning) |
| 22 | Journaled time & randomness syscalls (`sys.now`, `sys.random`) | M | S–M | **done** (worldDispatcher below replay; SDK now()/random()) |
| 23 | Multi-principal grants via attenuation tokens (macaroons) | H | L | open — D3 direction |
| 24 | Plan/execute split brain (CaMeL) | H | L | **done** (camel-brain: quarantined planner, $N variable routing) |
| 25 | Attempt-scoped idempotency keys across rollback boundaries | M–H | M | **done** (records carry the revision that wrote them; intent identity derives from the record) |

## 0. Ambient-surface lockdown

The kernel must own the guest's WASI sources instead of passing them through
(`RESEARCH.md` finding 1). Today determinism holds only because wazero's
default fake clock/RNG happen to be deterministic, and `extism:host/env
http_request` is unusable only because `AllowedHosts` happens to be empty —
four config fields away from silently breaking the determinism and
no-ambient-authority laws. Fix: construct the instance `ModuleConfig` inside
`NewKernel`/`CreateProcess` (pinned rand seed, pinned clocks, no env/args;
ignore caller-supplied ModuleConfig) and reject manifests with non-empty
`AllowedHosts`/`AllowedPaths` via a typed error. HTTP and files are
capabilities, not ambient rights. Extends #2's tests with: grantless guest
cannot reach HTTP/FS; clock/RNG reads identical across crash-replay.

## 1. Journal program-versioning + replay compatibility check

Record the program version/hash in each journal; on replay, verify it against
the running program and fail with a typed `ErrReplayIncompatible` instead of a
confusing divergence. Directly closes the "versioned-replay" fault line named
in `ARCHITECTURE.md` — the known hard problem of journal-replay systems — and
is cheapest to do now, while journals are young and small. Prerequisite for
safe brain evolution.

## 2. Kernel-law CI tests

Encode the five invariants as tests so they are provable rather than
aspirational, and so LLM-assisted development cannot silently drift:

- **determinism** — resume the same guest twice from the same journal and
  compare syscall sequences;
- **no ambient authority** — a guest whose dispatcher grants nothing can do
  nothing;
- **journal-before-observe** — a syscall result is committed before the guest
  can act on it;
- **un-bypassable approval** — an approval-required capability cannot execute
  without a resolved `Authorization`.

## 3. Hash-chained journal

Each record carries `hash(prev)`; the journal becomes tamper-evident. Elevates
the journal from replay tape to genuine audit artifact — durability and audit
as one mechanism, which is the point of the single-journal design.

## 4. Capability attenuation helper in `sys`

`Attenuate(parent, requested) ([]Capability, error)`, subset-only — the
delegation law from KeyKOS/seL4. Small on its own; prerequisite for #5.

## 5. `process.spawn` syscall

Sync-first child processes: `spawn(program, input, capabilities)` with

- capability subset enforced via #4 (a parent cannot grant what it lacks);
- deterministic child PID — `f(parentPID, spawnSeq, program)`, never random
  (determinism invariant);
- the child's result committed to the parent's journal, so on replay the child
  is not re-run;
- parent yields while the child runs (the "child workflow" pattern).

Unlocks multi-agent composition with an auditable authority tree. Defer async
spawn: it requires journaling every inter-process message as an ordered input
event, a real determinism cost to pay only when concurrency is needed. See the
spawn section of `ARCHITECTURE.md` for the full design discussion.

## 6. ABI version field

Add `"abi": 1` to the syscall envelope. Freezing the ABI (our POSIX) needs a
version to freeze against; trivially cheap now, painful to retrofit.

## Deferred (with reasons)

**7. Snapshot/checkpoint to bound replay cost** — the classic single-level-store
move, and the clean seam for future state migration. Deferred because replay
cost is not a real problem yet; building it now would be gold-plating
(`ARCHITECTURE.md` non-goals).

**8. Sources-as-inbound-drivers refactor in aurora-k8s-agent** — align the chat
sources with the driver symmetry (`ARCHITECTURE.md`, *Drivers: the symmetry*).
Deferred because aurora-k8s-agent is blocked on out-of-scope module migrations
(`aurora-capcompute`, `aurora-stores`) before it can adopt the renamed API at
all; do the refactor as part of that migration, not before.

## 9. Intent/completion journal records

Today the tape records a syscall only after the driver executed it: the
journal is write-ahead with respect to the guest but write-*behind* with
respect to the world (RESEARCH.md finding 8). Fix: append an **intent record**
before dispatch and a **completion record** after. Replay meeting an open
intent at the tail is a typed *indeterminate* condition (not divergence) with
per-capability policy; open intent + pending task = legitimately waiting. The
intent identity `(PID, position, call-hash)` doubles as an idempotency key
handed to drivers. Splits invariant #3 into two laws: journal-before-observe
(held today) and **journal-before-execute** (new). Cost: two appends per
effectful syscall; classify capabilities (`effectful` vs read) later.

## 10. Guest-registered compensation + abort-retry

`sys.begin`/`sys.commit` are **redo scopes**, not savepoints — they can only
re-execute, never undo, and re-execution amplifies at-least-once (RESEARCH.md
finding 9). The undo layer is guest-authored, in the log: `sys.compensate`
registers an effect's inverse (a deferred syscall, journaled with concrete
args), and `sys.abort{reason, retry_seconds}` rolls the open scope back —
registered compensations execute newest-first (journaled, idempotency-keyed,
crash-resumable), then the scope retries after the declared delay (forking at
its `sys.begin`) or the process stops as `compensated`. A failed compensation
fails the process with the rollback report; capabilities stay pure access
control (an earlier metadata-driven design was replaced by this one).

The rollback runs only when resuming is provably impossible — everything else
resumes. A host interruption and a guest **failure** alike re-drive by replay
under the *same revision*: recorded effects are served, an open intent
re-drives under its original key, and the registrations the cut-off guest was
about to make land in the journal — which is what makes registering an undo
*after* its effect safe (the rollback cannot run until every registration
reachable from the recorded history is durable). A failure whose re-drive
appends nothing has hit a deterministic wall; only then does the *implicit
abort* run: the host authors the same `sys.abort` record (with a `cause` the
guest cannot forge) and settles it exactly as a guest abort, before the
process reports failed. A **stop** rolls back immediately — the human asked
for an end, not a resume. A retry after either forks at the section's begin,
over compensated state, under a new revision — the only events that mint one.

## 18. Exactly-once effects: drivers honor idempotency keys

#9 gave every dispatch a deterministic idempotency key `(header, position,
call-hash)`, and the compensation/task re-drive paths already dispatch under
it — but leaf drivers do not dedupe on it, so the crash window between intent
and completion is still at-least-once, and a redo scope amplifies that.
Extend the contract: an effectful driver keeps an activity-memory of keys it
has executed (Helland, *Life beyond Distributed Transactions*, CIDR '07;
Stripe-style idempotency keys) and returns the recorded result on a re-seen
key. Start with the drivers that write (memory.put, internet POST when it
arrives); reads stay keyless.

## 19. Reservation / TCC as a pattern

Sagas have no isolation (García-Molina & Salem '87; Richardson's
countermeasures: semantic lock, pending state). The resolution: Try-Confirm/
Cancel needs **no kernel feature and no driver** — it is a usage pattern of
the primitives #10 already guarantees, because the pending state belongs to
the *participant*, not the orchestrator. In Pardon & Pautasso's RESTful TCC
the hold is a resource on the service that owns it; the coordinator only
remembers what to confirm or cancel — which is exactly what the journal and
`sys.compensate` already are. Aurora writes to third-party systems that every
reader treats as the source of truth: a reservation is only real if it is
written *there*. An orchestrator-side hold table (a `core.hold` driver
shipped briefly, then removed) is a reservation no other booker can see, and
an orchestrator-imposed TTL is the resource owner's policy usurped — wrong
twice for an architecture whose processes legitimately park for hours.

The pattern, inside a section:

	sys.begin
	hold  := dispatch("airline.reserve", args)          — a real write, visible to all readers
	         compensate({"airline.release", hold.id})   — the guaranteed undo (runs on abort, failure, or stop)
	…        payment, second leg, anything that may fail …
	         dispatch("airline.confirm", hold.id)       — the last call before commit
	sys.commit

No `confirm(&call)`-at-commit primitive either, by the same razor:
`compensate` earns its existence because abort/failure is a path the guest is
not alive to handle; commit is a path where the guest is alive and in
control, so a well-placed dispatch already does it. If the participant
self-expires holds, the guest handles the expired errno like any other
failure — their resource, their clock.

## 20. Approval-composable compensation

A rollback whose inverse yields (a refund over a threshold that needs
sign-off) currently fails the rollback. The settle loop is already
crash-resumable and the park/resume shape exists for forward calls; wire a
yielded inverse into a durable task, park the rollback, and resume settlement
on resolution — the human as terminal compensator *inside* the rollback
(WS-BPEL compensation-handler semantics), not only after it fails.

## 21. Deterministic rollback matrix

The strongest confidence-per-line investment available (FoundationDB's
simulation culture; TigerBeetle's VOPR). The sim harness (#14) crash-tests
forward replay; #10's backward path needs the same: crash at every append
position across register → abort → settle → park → fire → refork, asserting
exactly-once inverses, chain verification, and that a resumed settle never
re-runs a completed compensation. Rebuild the deleted unwind matrix for the
guest-registered semantics.

## 22. Journaled time & randomness syscalls

Guests must avoid wall clocks and RNGs today (the kernel pins them for
determinism, #0). Expose them as capabilities instead: `sys.now` and
`sys.random` journal their value like any completion and replay it verbatim —
Temporal's `workflow.Now`/`SideEffect` pattern. Removes a whole class of
guest landmines and enables guest-side backoff-with-jitter over the attempt
counter the input already carries.

## 23. Multi-principal grants via attenuation tokens

The D3 policy layer needs per-principal grant sets. The ceiling already
speaks `sys.Attenuate`; macaroons (Birgisson et al., NDSS '14) give
offline-attenuable bearer grants with contextual caveats (tenant, session,
expiry) that map one-to-one onto manifest ceilings — a multi-principal story
with no central ACL service, philosophically native to a capability system.

## 24. Plan/execute split brain

The enforcement substrate for prompt-injection resilience is done: labels,
flow policy, `sys.declassify` (#11). The missing half is brain-side — CaMeL
(Debenedetti et al., 2025, *Defeating Prompt Injections by Design*): a
privileged planner that never reads tool output emits a capability-checked
plan; a quarantined executor runs it over tainted data. The kernel's
capability + data-flow mediation is exactly the machine CaMeL assumes.

## 25. Attempt-scoped idempotency keys across rollback boundaries

Crash re-drive (same attempt: the key must be stable) and rollback retry (a
new transaction: the key must be fresh) are different beasts, and a key of
bare `(header, position, call-hash)` dressed them the same: a retry that
reproduced byte-identical args at the same position would *adopt* the
rolled-back attempt's recorded effect — an effect its rollback had already
compensated — instead of re-executing it. Resolved within the existing
abstractions: the revision **is** the attempt identity, and the fork
discipline already guarantees what the key needs. Each record carries the
revision that first wrote it (stamped by the Journal at append), and intent
identity derives from the record — `(header, revision, position, call-hash)`.
A re-driven open intent sits in the fork's shared prefix with its origin
revision intact, so it recomputes its original key however many resume forks
intervene; a rolled-back section's retry re-executes live, appends fresh
records under the new revision, and gets a fresh key space. Uniform for
children too — their journals scope their own keys. Proven by the crash
matrices (re-drive stability) and a recharge test (a deduping driver takes
the retry's identical charge as a new effect, never the compensated one).
