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
| 3 | Hash-chained journal (tamper-evident audit) | M–H | S | open |
| 4 | Capability attenuation helper in `sys` | M | S | **done** (`sys.Attenuate`) |
| 5 | `process.spawn` syscall (sync-first child processes) | H | M | open |
| 6 | ABI v2 bundle: version field, errnos, savepoint syscalls | M | S | **done** (`sys.ABIVersion=2`, `sys.Errno`, `sys.SyscallBegin/Commit`) |
| 7 | Snapshot/checkpoint to bound replay cost | M | L | deferred |
| 8 | Sources-as-inbound-drivers refactor (aurora-k8s-agent) | M | M | deferred |
| 9 | Intent/completion journal records (journal-before-execute) | H | M | open — next |
| 10 | Compensation metadata + saga unwinding | H | M | open — after #9 |
| 11 | Information-flow labels + provenance (CaMeL-style) | H | L | open — frontier (CHALLENGE.md A) |
| 12 | Resource management (mem cap, resume deadline, aggregate quotas) | H | S–M | open (CHALLENGE.md B) |
| 13 | Reference-monitor validation (grant-set + InputSchema) | H | S | open — cheap (CHALLENGE.md C) |
| 14 | Deterministic simulation testing harness | H | M | open (CHALLENGE.md D) |
| 15 | Scheduler seam: priority, admission, virtual-actor activation | M | M | open (CHALLENGE.md F) |
| 16 | Journal lifecycle: snapshot + compaction + retention | M | M | open — supersedes #7 (CHALLENGE.md G) |
| 17 | Journal→OpenTelemetry exporter | M | S | open — cheap (CHALLENGE.md H) |

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

## 10. Compensation metadata + saga unwinding

`sys.begin`/`sys.commit` are **redo scopes**, not savepoints — they can only
re-execute, never undo, and re-execution amplifies at-least-once (RESEARCH.md
finding 9). Add the missing layers: a declared **compensation** on
`sys.Capability` (inverse syscall or explicit cannot-compensate); kernel-level
**saga unwinding** — on abort of a scope, dispatch completed effects'
compensations in reverse, journaled and composable with approval; TCC-shaped
reservations where the effect side supports drafts/dry-runs; and escalation
to the human (with the journal) as the first-class terminal compensator.
Depends on #9 for the completed-effect records unwinding walks.
