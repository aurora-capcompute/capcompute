# Roadmap — improvements that flow from the OS model

Scored follow-ups now that the API speaks the OS vocabulary (see
`ARCHITECTURE.md` for the model and its invariants). Score = impact on
coherence/capability (H/M/L) × effort (S/M/L). The order below is the
recommended sequence; each item is deliberately small enough to land alone.

| # | Item | Impact | Effort |
|---|------|--------|--------|
| 1 | Journal program-versioning + replay compatibility check | H | S–M |
| 2 | Kernel-law CI tests (the five invariants as tests) | H | S–M |
| 3 | Hash-chained journal (tamper-evident audit) | M–H | S |
| 4 | Capability attenuation helper in `sys` | M | S |
| 5 | `process.spawn` syscall (sync-first child processes) | H | M |
| 6 | ABI version field in the syscall envelope | M | S |
| 7 | Snapshot/checkpoint to bound replay cost | M | L — deferred |
| 8 | Sources-as-inbound-drivers refactor (aurora-k8s-agent) | M | M — deferred |

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
