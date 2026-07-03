# OS-research review — what Aurora reinvented, and what to adopt

This document audits Aurora's invented mechanisms against operating-systems
research and modern OS practice. For each mechanism: what the code does today
(with evidence), the prior art that names the same problem, a verdict, and the
concrete change if one is warranted. Findings are ranked by value.

Companion docs: `ARCHITECTURE.md` (the OS model and its five laws),
`ROADMAP.md` (execution order; updated to reflect this review).

Scope note: several mechanisms live in the `aurora-capcompute` runtime module
and `aurora-k8s-agent`; they are reviewed here because this library defines the
laws they must uphold.

---

## 1. The ambient-authority surface is open — determinism holds by accident

**Severity: this is the one finding that threatens the kernel laws.**

**Today.** Every production call site enables full WASI and configures nothing
else (`extism.PluginConfig{EnableWasi: true}`, no `AllowedHosts`, no
`AllowedPaths`, nil `InstanceConfig.ModuleConfig`). That instantiates the
complete `wasi_snapshot_preview1` table — `clock_time_get`, `random_get`,
`environ_get`, `poll_oneoff`, `fd_*` — plus Extism's always-on
`extism:host/env` module, which exports `http_request`, `config_get`,
`var_get/var_set`, and `get_log_level` to every guest. None of this flows
through the journaled syscall dispatcher.

Replay determinism currently survives only because of defaults: wazero backs
unset sources with **deterministic fakes** (RNG = `math/rand` seeded 42; clock
= 2022-01-01 advancing 1ms per read), the filesystem is unmounted, and
`http_request` panics because `AllowedHosts` is empty. So the guest cannot
*today* observe real nondeterminism — but:

- the fakes are **call-count-sensitive** and their reads are **not journaled**;
  correctness rests on replay re-executing a byte-identical code path;
- four pass-through config fields (`InstanceConfig.ModuleConfig` with
  `WithSysWalltime`/`WithSysNanotime`/`WithRandSource`/`WithEnv`,
  `Manifest.AllowedPaths`, `Manifest.AllowedHosts`, and the host env var
  `EXTISM_ENABLE_WASI_OUTPUT`) each silently convert a fake into real,
  un-journaled nondeterminism or real ambient authority;
- `http_request` cannot be un-exported — an `AllowedHosts` entry opens
  un-journaled outbound HTTP that bypasses the dispatcher, the replay tape,
  the audit journal, and `require_approval` entirely;
- `get_log_level` reads a process-global mutable atomic — a small
  nondeterministic read no config can close.

**Prior art.** This is the *ambient authority* problem, the founding concern of
capability security (Dennis & Van Horn 1966; KeyKOS; seL4). The modern systems
answer is uniform: **Capsicum** (USENIX Security '10) — once in capability
mode, no ambient namespaces exist; **CloudABI** — delete ambient POSIX
entirely, everything arrives as a descriptor; **Fuchsia** — no ambient
syscalls, all authority via handles. On the determinism side, **Determinator**
(OSDI '10) and **dOS** (OSDI '10) established that determinism must be
*enforced by the kernel*, not assumed of well-behaved programs; FoundationDB's
simulation discipline and Antithesis re-taught the same lesson commercially.
WASI preview1's ambient clocks/RNG are a known regression from its CloudABI
ancestry — preview2 re-capabilizes them.

**Verdict: adopt now (kernel change, small).** The kernel must *own* the guest
source configuration rather than pass it through:

1. In `NewKernel`/`CreateProcess`, construct the instance `ModuleConfig`
   internally: pin `WithRandSource` to a seed derived from the PID (or a
   journaled seed), pin walltime/nanotime to fixed or journaled sources, no
   env, no args. Ignore or reject a caller-supplied `ModuleConfig`.
2. Validate the manifest at kernel construction: reject non-empty
   `AllowedHosts` and `AllowedPaths` with a typed error. Guests that need HTTP
   or files get them as *capabilities* through the dispatcher — that is the
   whole point of the architecture.
3. If real time/randomness is ever needed by a brain, expose it as journaled
   syscalls (`sys.clock`, `sys.random`) — Temporal's `workflow.Now` rule.
4. Add kernel-law tests (ROADMAP #2): a grantless guest must fail to reach
   HTTP/FS; clock/RNG reads must be identical across a crash-replay.

---

## 2. Durable tasks are ports — the unification is right; finish it

**Today.** Approvals and timers converged on one primitive: a capability yield
becomes a persisted `task.Record` identified by `(scope, journal position,
call-hash)`, resolved later by a human (approval card) or the timer scheduler,
which resumes the run through the same replay path. This is genuinely good
design — one durable wait primitive, multiple resolvers.

Two seams show the primitive isn't first-class yet: the timer scheduler
recognizes its tasks by **string-matching the capability name**
(`IsTimerTask`), and task interception happens by position in the dispatcher
chain with delegation yields special-cased by name prefix.

**Prior art.** The unified object is a **port/durable promise**: Mach ports,
Zircon ports/events (`zx_object_wait`), KeyKOS kernel keys, Golem's durable
promises. The OS lesson is that *the waitable object is the primitive* and
resolvers are interchangeable; consumers never dispatch on the name of what
created it.

**Verdict: adopt later (naming + one field, cheap).** Give `task.Record` an
explicit `Kind` (or `Resolver`) field set at creation, so the timer scheduler
and bridges select on a typed field instead of parsing capability names. When
`process.spawn` lands (ROADMAP), a child-process completion becomes just
another resolver of the same object — the special-casing of delegation yields
can collapse into a kind. A multi-wait (`poll`-style) syscall is *not* needed
under cooperative single-yield semantics; don't build it (non-goals).

## 3. `host.try`/`host.commit` reinvented transaction brackets — make them real syscalls

**Today.** Guests bracket must-not-repeat units with reserved marker
capabilities; a `savepointDispatcher` journals them as no-ops, and failed-run
resume forks the journal after the outermost unclosed `host.try` so the whole
unit re-executes. The brackets are magic *string names* smuggled through the
capability namespace, and nesting semantics are implicit (outermost-open only).

**Prior art.** Write-ahead intent + recovery-by-bracket is the WAL/savepoint
discipline (ARIES; SQL `SAVEPOINT` with stack semantics); at the OS level,
Argus (Liskov's atomic actions) and IBM's QuickSilver (transactional OS)
made process-level units first-class. The "magic name in a generic namespace"
pattern is the `ioctl` lesson: escape hatches accrete semantics that belong in
the ABI.

**Verdict: adopt later (ABI v2 item).** Promote savepoints to explicit
syscalls (`sys.begin`/`sys.commit`, or fields on `Syscall`), with defined
nesting (a savepoint *stack*, as in SQL). Do it together with the ABI version
field (ROADMAP #6) since it is a wire change. The recovery algorithm itself is
sound — keep it.

## 4. Capability grants: structural subtrees are KeyKOS-shaped; add a rights algebra and real revocation

**Today.** The Manifest tool tree *is* the grant — a child agent receives a
literal subtree, so attenuation is structural (you can only hand down what is
written under you). Subset checks on settings are **bespoke per registration**
(`IsSubset` for internet origins, MCP tools; everything else demands byte
equality). Revocation is the policy-digest rotation: any policy change stops
the active run and rotates the whole session.

**Prior art.** Subtree-as-grant matches capability-list construction
(KeyKOS/seL4): authority is what you were handed, nothing ambient — validated
design. What research adds: (a) a uniform **attenuation algebra** — seL4
badges/rights bits, Fuchsia `zx_handle_replace` with reduced rights — instead
of N bespoke subset checks; (b) **revocation by indirection/epoch**
(KeyKOS/EROS): revoke a grant without killing the world. Thread rotation is
the capability equivalent of "revoke by killing the process" — correct but
maximally coarse. (c) The `Hidden` flag is the *facet* pattern from object
capabilities — fine as is.

**Verdict: adopt with #4/#5 of the roadmap.** The planned `sys.Attenuate`
helper should define the uniform contract (`child ⊑ parent`, property-tested)
that registrations implement, replacing ad-hoc `IsSubset` growth. For
revocation, add a **grant generation** (epoch) to the dispatcher: bump on
policy change, check at each dispatch — mid-run revocation becomes possible
and session rotation becomes the fallback, not the only tool.

## 5. Task tokens are unnamed Macaroons — validated, one cheap upgrade available

**Today.** Approval tokens are derived (`HMAC(secret, tenant·task)`), never
stored (only the hash is), verified constant-time, encrypted at rest in the
bridge, expiry enforced host-side, ownership checked against the resolving
user. This is a bearer capability token done properly.

**Prior art.** SPKI/bearer-capability practice; **Macaroons** (NDSS '14) are
the named version — HMAC-chained tokens with *caveats* enabling offline
attenuation ("valid only for chat X", "before time T") without server state.

**Verdict: keep; adopt caveats only if tokens ever leave the bridges** (e.g.
webhook approval links). Then Macaroon-style caveats are the standard answer
and nearly drop-in given the HMAC construction already used.

## 6. Errors are prose — give failures an errno

**Today.** `sys.Fail(message string)`: failures carry only a human string.
Guests branch on status; the brain treats any failure as recoverable unless it
marked the call `"hard"`. Nothing machine-readable distinguishes *denied* from
*expired* from *not-found* from *transient*.

**Prior art.** Half a century of errno/HTTP status codes; Midori's error-model
retrospective (bugs vs. recoverable errors as distinct categories) is the
modern statement. Strings-as-errors invite guests to parse prose — the
compatibility trap POSIX solved with numbered errors.

**Verdict: adopt with ABI v2 (small).** Add an optional `code` field to the
failure result (small closed set: `denied`, `expired`, `not_found`,
`invalid_args`, `transient`, `internal`). Keep the message for humans. Ship
alongside the ABI version field so it is one wire change.

## 7. Validated by research — keep as is (now with names)

- **Event-sourced runtime, one goroutine per active run** — actor-per-run over
  an append-only log folded into projections: the BEAM/event-sourcing shape.
  One-active-run-per-session with `ErrConflict` is foreground job control.
- **Inbox idempotency** — durable insert before offset advance, claim/complete
  for callbacks: the transactional-inbox pattern, correctly done, including
  the benign race between timer and human resolver (single-resolver lease,
  loser gets `ErrConflict`).
- **Brain ABI versioning** — artifact declares `abi`, host rejects mismatch at
  OCI pull: exec-format checking (ELF ABI version), enforced at load time
  where it belongs. Extend into the journal per ROADMAP #1 (journals should
  record the artifact digest they were written by — brain IDs are already
  digest-namespaced).
- **Copy-on-write journal revisions on retry** (`forkJournalLocked`) — this is
  checkpoint forking; the same seam ROADMAP #7's snapshots will use.
- **The brain's actions-array protocol** — batched submissions with aggregated
  completions is the io_uring shape, already discovered at the protocol layer.
  The boundary is in-process, so there is no crossing cost to amortize; do
  *not* move batching into the kernel ABI (non-goals).

## 8. The journal is write-behind with respect to the world — adopt intent records

**Today.** The replay tape records a syscall only *after* the driver executed
it: tape check → execute the side effect → `Record`. Invariant #3
(journal-before-observe) holds for the **guest** — it never acts on an
unrecorded result — but with respect to the **world** the journal is
write-*behind*. The crash window between execute and commit has three
consequences: (a) duplicate execution on replay (at-least-once; the README
already admits the cancellation case); (b) "never attempted" and "attempted,
outcome unknown" are indistinguishable — both are an absent record; (c) an
executed-but-uncommitted action is invisible to the audit trail, a hole
exactly where the governance value proposition lives.

**Prior art.** The ARIES/WAL rule: log the intent before performing the
action. Temporal journals **two events per activity** — `ActivityTaskScheduled`
(intent) then `ActivityTaskCompleted` (outcome). Two-generals makes
exactly-once external effects impossible; the achievable contract is *visible*
at-least-once, upgraded to effectively-once via an **idempotency key** the
downstream can deduplicate on. The task layer already computes the right key:
`(PID, journal position, call-hash)`.

**Verdict: adopt (highest-value open item).** The tape appends an **intent
record** before dispatch and a **completion record** after. Replay meeting an
open intent at the tail is a new typed condition — *indeterminate*, not
divergence — with per-capability policy (fail for human review on mutations;
retry on reads). Disambiguation: open intent + pending task = legitimately
waiting; open intent + no task = indeterminate. The intent identity doubles as
an idempotency key handed to drivers. Splits invariant #3 into two laws:
*journal-before-observe* (guest side, held today) and **journal-before-execute**
(world side, the new one). Cost: two appends per effectful syscall — classify
capabilities (`effectful` vs read) later; intent-log everything first.

## 9. Savepoints are redo scopes, not transactions — add the missing layers

**Today.** `sys.begin`/`sys.commit` bracket a unit; on failed-run resume the
journal forks past the outermost open bracket and the unit **re-executes**.
That is a *redo scope* (checkpoint-restart), not a savepoint: SQL savepoints
promise rollback — undo — and this mechanism cannot undo anything. Worse,
bracket recovery *deliberately re-executes possibly-completed effects*, so
brackets amplify the at-least-once problem of finding 8 and are only safe over
idempotent contents — which nothing enforces.

**Prior art — the problem has a four-layer answer, and Aurora has one layer:**
1. *Effectively-once floor*: intent records + idempotency keys (finding 8).
2. *Real transactions for system-owned state only*: Beldi (OSDI '20), Boki
   (SOSP '21), Halfmoon (SOSP '23), Styx — exactly-once/ACID by keeping state
   inside the system and paying 2PC/locking latency. Fundamentally cannot
   cover external effects (a k8s apply, a sent message).
3. *External effects*: **Sagas** (Garcia-Molina & Salem 1987) — each step
   declares a **compensating action**; on abort, completed steps are
   compensated in reverse. The canonical, industry-converged answer (Temporal,
   BPEL). Compensation is *declared, never inferred* — no research solves
   automatic inverse-derivation. Where the downstream cooperates,
   **Try-Confirm/Cancel** reservations beat compensation (reserve → confirm).
4. *The agent frontier*: SagaLLM (VLDB 2025) applies sagas to multi-agent LLM
   planning; checkpoint-restore security work (ACRFence) states the boundary
   that applies to our journal-fork exactly: restoring process state **cannot
   undo actions already performed on external services**.

**Verdict: keep brackets, reframe them honestly, add compensation.**
- Document `sys.begin`/`sys.commit` as **redo scopes**; bracket re-execution
  should require the finding-8 idempotency floor (flag brackets over
  non-idempotent, un-keyed effects).
- Add declared **compensation to `sys.Capability`** (the inverse syscall, or
  an explicit cannot-compensate marker). The kernel gains **saga unwinding**:
  on abort of a scope, dispatch completed effects' compensations in reverse —
  each journaled, audit-visible, and composable with approval (compensating an
  approved action can itself require approval).
- **TCC maps onto the approval flow** — `require_approval` is already
  reserve-then-confirm on the human side; extend the shape to the effect side
  (drafts/dry-runs confirmed after approval).
- **The human is the terminal compensator**: the last rung of every
  compensation ladder is escalation with the journal of what happened — make
  it a first-class outcome, not an implicit failure.

Strategic note: kernel-level, capability-declared compensation with governed
unwinding exists in the literature only as patterns and papers (SagaLLM is not
a runtime). This layer is genuinely near the frontier.

## Apply order

1. ~~**Ambient-surface lockdown (finding 1)**~~ — done (ROADMAP #0).
2. ~~ROADMAP #1 (journal versioning)~~ done; #2 partial (laws 1/2 tested).
3. ~~ABI v2 bundle (findings 3 + 6)~~ — done.
4. **Intent/completion records (finding 8)** — ROADMAP #9; the
   highest-value open item: closes the audit hole and provides the
   idempotency floor that findings 9 and spawn both assume.
5. **Compensation metadata + saga unwinding (finding 9)** — ROADMAP #10;
   after #9 (unwinding walks completed-effect records).
6. Task `Kind` field (finding 2) and attenuation-at-grant + epochs
   (finding 4) — with the runtime migration.
