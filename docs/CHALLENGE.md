# Adversarial audit — where Aurora is not the state of the art

A thorough challenge of the whole system against OS and distributed-systems
research and the 2025–2026 agent-security frontier. Each finding: **what it is**
today, **why it's weak** (or why it's a defensible tradeoff, stated honestly),
the **state of the art** with named prior art, and a **plan**. Companion to
`RESEARCH.md` (findings 1–9, already partly implemented) and `ROADMAP.md`.

Honesty rule kept throughout: some of these are genuine gaps, some are
defensible choices that only need documenting, and a few are frontier bets
where the research exists only as papers. Each is labelled.

## Ranked summary

| # | Finding | Kind | Severity | On-thesis |
|---|---------|------|----------|-----------|
| A | No information-flow control / data provenance (prompt-injection frontier) | gap | **critical** | ★★★ |
| B | No resource management (CPU/memory/quota metering) — the missing OS half | gap | **high** | ★★ |
| C | Reference monitor doesn't validate syscall args / authorization (confused deputy) | gap | **high** | ★★★ |
| D | Determinism is a law but unused for testing (no DST) | gap | high | ★★ |
| E | JSON-string syscall ABI vs typed/component-model interfaces | weak abstraction | medium | ★ |
| F | Scheduling: no fairness, admission control, priority, or activation | gap | medium | ★ |
| G | No journal lifecycle: compaction, GC, retention | gap | medium | ★★ |
| H | No observability / trace export (the journal is an unused trace) | gap | medium | ★ |
| I | IPC + supervision unspecced for the multiprocess future | spec debt | medium | ★ |
| J | Capabilities are authorized-by-name, not unforgeable references | classification | low | ★ |

---

## A. No information-flow control / data provenance — CRITICAL, and the frontier

**What it is.** Capabilities gate *what* a guest may call. Nothing tracks *where
a value came from* or *where it may flow*. A guest that reads secret data through
one capability can pass it as an argument to another — exfiltration through a
legitimately-granted capability. And the guest is an LLM whose inputs include
tool outputs, which are attacker-controllable: indirect prompt injection flows
straight into the logic that chooses the next syscall.

**Why it's critical — and exactly on-thesis.** For a system whose pitch is
*governed, safe agent actions*, the capability model is necessary but not
sufficient: it cannot stop a granted capability from being used *with tainted
data* or *to leak tainted data*. This is the "lethal trifecta" (private data +
untrusted content + an exfiltration channel) and capabilities alone don't break
it. This is the single biggest gap between Aurora and the security it claims.

**State of the art (2025).** [CaMeL](https://arxiv.org/pdf/2503.18813) (Google
DeepMind, "Defeating Prompt Injections by Design") is *Aurora's own thesis
applied to injection*: it treats prompt-injection defense as a **systems**
problem, borrowing Control-Flow Integrity + Access Control + **Information Flow
Control**. Concretely: (1) attach **capability metadata to every value** to
restrict how it flows; (2) **separate control flow from data flow**; (3) a
**dual-LLM** split — a *privileged* LLM plans from trusted input, a *quarantined*
LLM processes untrusted data with no tool access; (4) a deterministic
**interpreter/reference monitor** checks policy before every tool call. A whole
family now exists — CaMeL, FIDES, Progent, RTBAS, FORGE — all enforcing security
*outside the model with a deterministic mediator*, which is **precisely what
Aurora's dispatcher already is**. The OS lineage underneath is DIFC: HiStar,
Flume, Asbestos, DStar — labels/taint on data with **declassification as an
explicit privileged operation**.

**Plan (staged frontier bet).**
1. **Provenance labels on results.** Every `SyscallResult` carries a taint set
   (which capabilities/sources it derived from — e.g. `untrusted_web`,
   `secret`). The dispatcher stamps them; the journal records them (they become
   part of the audit trail — provenance for free).
2. **Flow policy at the next syscall.** Because the guest (LLM) is an opaque box,
   label propagation lives at the dispatcher boundary, not inside the guest: the
   reference monitor rejects a syscall whose args carry labels the target
   capability forbids (`k8s.delete` may not receive `untrusted_web`-tainted args
   without declassification). This is CaMeL's move, minus needing to instrument
   the guest.
3. **Declassification = a governed operation** — composes with `require_approval`
   (a human authorizes moving a value across a label boundary).
4. **Dual-LLM / CFI (brain-architecture).** Trusted plan from the user prompt;
   quarantined processing of tool outputs. This is a brain change, the deepest
   part, and where the real robustness lives.

This is the item most worth doing: it converts "we gate actions" into "we track
and gate *data flow* across actions" — the actual frontier, and no agent runtime
ships it as a kernel primitive. New ROADMAP #11.

## B. No resource management — the missing half of "operating system"

**What it is.** No memory limit, no CPU metering, no quotas. `NewKernel` sets
only `WithCloseOnContextDone(true)` — a guest is stoppable only by *external*
context cancel. A runaway guest loops or allocates unbounded (up to wazero's
default address space); there is no per-`cred` accounting and one wazero runtime
is shared across all processes (noisy neighbour).

**Why it's a gap.** An OS is a *resource manager* **and** an extended machine
(Tanenbaum). Aurora built the extended machine and skipped the resource manager.
For a multi-tenant governance substrate this is a denial-of-service and fairness
hole: one tenant's burst starves others; a buggy brain in an infinite loop is
only killable by someone noticing and calling `Stop`.

**State of the art.** wasmtime offers two complementary mechanisms:
**fuel** (a deterministic per-execution instruction budget — traps on
exhaustion; *deterministic*, so it composes with the replay law and could make
CPU part of replayable state) and **epoch interruption** (async wall-clock
preemption at compiler-inserted safe points). Plus `ResourceLimiter`
(per-`Store` memory/table caps). The multi-tenancy literature flags the trap
Aurora would hit: per-call limiters miss *aggregate* — 1000 concurrent calls at
the cap = 1000× the cap; you need **per-tenant aggregate accounting**
(cgroups-style hierarchical limits).

**Plan.** wazero is weaker here than wasmtime (it has memory limits and
context-deadline interruption but **no fuel metering**). So, honestly staged:
1. **Now (wazero-native):** set a per-process memory cap (`RuntimeConfig`
   memory-limit pages) and a per-`Resume` wall-clock deadline (you already
   cancel on context) — closes unbounded-memory and infinite-loop today.
2. **Aggregate quotas:** a per-`cred` accounting layer in the scheduler seam
   (finding F) — bytes, syscalls/sec, concurrent processes.
3. **Deterministic CPU budget (frontier):** true *fuel* would make CPU part of
   the journaled state (replay-exact resource use) but needs wasmtime or a
   wazero fuel shim — flag as the deterministic-metering bet, not v1.

New ROADMAP #12.

## C. The reference monitor doesn't validate args or authorization — confused deputy

**What it is.** At the capcompute layer, `Dispatch(ctx, cred, syscall, auth)`
routes by `syscall.Name`. It does **not** verify (a) that the args conform to the
capability's declared `InputSchema`, nor (b) that `cred` actually *holds* the
named capability. Both checks currently live (if at all) in the app's dispatcher
chain (the runtime's guarded dispatcher / manifest grant tree).

**Why it's a gap.** A reference monitor that trusts its caller's input is a
confused deputy. If any dispatcher in the chain routes a name without checking
the cred's grant set, that's privilege escalation by string; if it forwards
unvalidated args, a driver executes attacker-shaped input. Saltzer & Schroeder's
*complete mediation* says the monitor must validate **every** access.

**State of the art.** Complete mediation (Saltzer & Schroeder 1975); schema
validation at the trust boundary (every serious RPC framework validates against
the declared contract before dispatch); capability possession checked by the
kernel, not asserted by the caller.

**Plan (cheap, high-value).** Provide a kernel-side **validating decorator**:
before delegating, (1) check `cred`'s grant set contains `syscall.Name` (fail
`ErrnoDenied`), and (2) validate `syscall.Args` against the capability's
`InputSchema` (fail `ErrnoInvalidArgs`). Make it a first-class piece of the
chain so no app can forget it. This is the "reference monitor validates its
inputs" law, enforced in code. New ROADMAP #13.

## D. Determinism is a kernel law — but unused for testing (no DST)

**What it is.** Determinism is enforced (`ambient.go`, kernel law #2) yet tests
are conventional unit/integration. The entire *point* of paying the determinism
tax is that it **enables** deterministic simulation testing — and that payoff is
left on the table.

**Why it's a gap.** [FoundationDB, TigerBeetle, Antithesis, Resonate,
WarpStream](https://notes.eatonphil.com/2024-08-20-deterministic-simulation-testing.html)
show a deterministic system should be tested by *simulating years of
fault-injected operation in minutes and replaying any failure exactly*. Aurora
is architecturally ready (deterministic guests, journaled I/O, pinned clock/RNG)
and using none of it. This is also the natural test home for findings 8/9
(intent records / compensation) whose whole difficulty is crash timing.

**State of the art.** DST: control every nondeterminism source (already done),
inject faults (crash-before-commit, crash-after-execute, resolver races,
message reordering), drive a deterministic scheduler, and assert invariants
(replay convergence, effect idempotency, no orphaned intents) across the whole
fault matrix.

**Plan.** A simulation harness driving the kernel with a mock `ProcessTable`
and a fault-injecting dispatcher; script crashes at *every* journal position;
assert the finding-8/9 invariants hold across the matrix. Few agent runtimes can
DST because few are deterministic — this is both a robustness multiplier and a
differentiator. New ROADMAP #14.

## E. The JSON-string syscall ABI is a weak abstraction — but the fix is a real tradeoff

**What it is.** Syscalls are JSON blobs over a linear-memory pointer: untyped,
serialized every call, no compiler-checked guest/host contract, per-call schema
evolution ad hoc.

**Why it's weak (and where it's fine).** Against the **WebAssembly Component
Model + WIT** (typed, language-agnostic interfaces — what Golem uses), this is
"stringly-typed syscalls": nothing checks the contract at build time and every
schema mismatch is a runtime error. *But* JSON-over-pointer is also why capcompute
is a tiny embeddable library instead of a toolchain — a real part of its posture.

**State of the art.** WIT / component-model typed imports; typed capability
interfaces generated for each guest language.

**Plan (flag, don't mandate).** Two independent moves: (1) the cheap, unambiguous
half is finding C's host-side `InputSchema` validation — get contract-checking at
runtime without a toolchain. (2) Migrating the ABI to the component model is the
Golem convergence: real typing, but it trades away the simple embeddable posture
and is a large change. Evaluate as a deliberate fork, not a default. No roadmap
number until (1) ships and the tradeoff is decided.

## F. Scheduling: no fairness, admission control, priority, or activation

**What it is.** One goroutine per active run; one-active-run-per-thread;
scheduling policy pushed entirely to the app. No priority, no fair queueing
across tenants, no admission control / backpressure, no memory-bounding
activation (idle processes stay resident).

**Why it's a gap.** Fine at low load; under contention there is no fairness or
overload protection, and no way to prioritize an interactive approval over a
batch run. Pushing *policy* to userland is the right microkernel instinct, but
the *seam* is too thin to express these.

**State of the art.** CFS-style fair scheduling and scheduler classes;
admission control / load shedding (SEDA); **virtual actors** (Orleans,
Cloudflare Durable Objects) and Golem's suspend-to-zero: single-threaded per
entity, but with managed placement and **activation/deactivation** so idle
entities evict from memory and reactivate on demand.

**Plan.** Keep policy in userland but widen the seam: a `Scheduler` interface
with priority + admission hooks (also needed by spawn — children must be
scheduled); ship a default fair-share scheduler; adopt virtual-actor activation
to bound resident memory. Pairs with finding B's aggregate quotas. New ROADMAP
#15.

## G. No journal lifecycle — compaction, GC, retention

**What it is.** The per-thread append-only log grows unbounded. Snapshotting is
deferred (ROADMAP #7); there is no compaction, no retention policy, no GC of
terminated runs.

**Why it's a gap.** Unbounded growth is an operational time bomb and makes
replay-from-zero cost grow linearly. As an *audit* artifact you also owe a
retention/archival policy (how long, tamper-evident cold storage).

**State of the art.** Log compaction (Kafka); snapshot-and-truncate with fuzzy
checkpoints (ARIES, Raft); tiered retention; WORM archival for audit. The
hash-chain (ROADMAP #3) makes archived segments independently verifiable.

**Plan.** Promote snapshotting (#7) from "deferred" the moment journals are
real, and pair it with compaction + a retention policy; verifiable archived
segments via the hash-chain. New ROADMAP #16 (supersedes the deferral of #7).

## H. No observability — the journal is a perfect trace, unexported

**What it is.** The journal *is* a complete execution trace, but nothing exports
it. No OpenTelemetry, metrics only where the bridge happens to log. Golem shipped
OTLP.

**State of the art.** OTel spans/traces; the journal maps cleanly to a span tree
(syscall = span, yield = async boundary, cred = resource attributes). Metrics:
per-cred resource use, syscall latency, approval-queue depth.

**Plan.** A journal→OTel exporter — cheap, high operational value, and it makes
the governance/audit story legible to tooling teams already run. New ROADMAP #17.

## I. IPC + supervision unspecced for the multiprocess future

**What it is.** Processes can't talk to each other; spawn is sync-only (planned).
`ARCHITECTURE.md` names OTP supervision as prior art but specs no restart
taxonomy, crash-propagation policy, or orphan handling.

**State of the art.** Actor mailboxes (BEAM, Orleans); capability-passing
message send/receive (seL4 endpoints, Cap'n Proto — capabilities *are* the refs
passed); OTP supervision strategies (one-for-one / one-for-all / rest-for-one,
max-restart-intensity, "let it crash"). Deterministic cross-process replay:
journal every message as an ordered per-receiver input event.

**Plan (spec now, build with spawn — non-goal discipline).** When multiprocess
lands: IPC = capability-passing message send/receive, each journaled;
deterministic interleaving via per-receiver ordered input log; supervision as
process metadata (restart strategy, crash propagation, orphan handling). Fold
into the spawn design in `ARCHITECTURE.md`; no separate roadmap number until
spawn forces it.

## J. Capabilities are authorized-by-name, not unforgeable references — classification, not bug

**What it is.** A capability is a *string name*. Security rests on the reference
monitor refusing names not in `cred`'s grant set (once finding C lands) — not on
the guest possessing an unforgeable token.

**Why it's mostly fine.** True object-capability systems (seL4 cspaces, Cap'n
Proto sturdy refs) make *possession = authority* via kernel-protected,
unforgeable references. Aurora's model is ACL-flavoured: the guest names a
capability, and the monitor checks the *cred's* grant set — the guest cannot
forge authority because authority lives in the cred, not the name. That is sound,
but it is **not** the unforgeable-reference model and should be documented as
such so no one assumes properties it lacks (e.g. capability *delegation between
guests* by passing a token — that needs real refs, and becomes relevant with
IPC/spawn).

**Plan.** Document the model honestly in `ARCHITECTURE.md` ("authorized-by-cred,
not by unforgeable token"). Revisit only if guest-to-guest capability delegation
(finding I) is needed — then unforgeable refs become worth the cost.

---

## Recommended order (interleaved with existing roadmap)

Ordered by value × cost, honest about dependencies:

1. **C — reference-monitor validation** (cheap, closes a real security hole).
2. **B step 1 — memory cap + resume deadline** (cheap, closes DoS).
3. **ROADMAP #9 intent records → #10 compensation** (already next; DST is their
   test home).
4. **D — deterministic simulation testing harness** (turns the determinism law
   into its payoff; tests #9/#10).
5. **A — information-flow labels** (the frontier bet; the biggest differentiator;
   stage it: labels+policy first, dual-LLM later).
6. **H — journal→OTel** (cheap operational win).
7. **F / B step 2 — scheduler seam + aggregate quotas**, then **G — lifecycle**.
8. **E, I, J** — decide/spec as their dependencies (typing, spawn) arrive.

The through-line: Aurora is a genuinely strong *extended machine* with a real
capability reference monitor — near the CaMeL family's frontier by construction.
Its gaps cluster in the *other* half of an OS (resource management, scheduling,
lifecycle) and in the *data-flow* dimension of security (provenance/IFC) that
capabilities alone can't cover. Closing A and B is what would make the
"governed execution" claim fully true rather than half-true.
