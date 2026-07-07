# Roadmap

The OS model is built and adversarially proven (see the ledger at the bottom;
`ARCHITECTURE.md` for the model and its invariants, `RESEARCH.md` for the
prior-art review). The current focus is getting it to production in the
narrowest honest posture: **a single-tenant deployment with one communication
channel — the `/v1` HTTP API — as the trust boundary**. One organization, one
runtime instance, one SQLite file, one API surface. Multi-tenancy, additional
channels, and scale-out are deliberately parked (see *Out of scope*), because
their hard parts — per-principal grants, pooled stores, placement — only pay
off once a single tenant runs in production.

Score = impact (H/M/L) × effort (S/M/L). Items are sequenced and small enough
to land alone.

## Now: production, single-tenant, one channel

| # | Item | Impact | Effort | Status |
|---|------|--------|--------|--------|
| 26 | Channel boundary: bearer auth on `/v1` | H | S | open |
| 27 | HTTP server hardening (timeouts, limits, `/healthz`) | M | S | open |
| 28 | Durability ops: backup + tested restore | H | S–M | open |
| 29 | Operational metrics, alerts, runbook | H | M | open |
| 30 | LLM spend budget per process | M–H | S | open |
| 31 | Deploy shape: single-writer posture, manifests, playbooks | M | S | open |

### 26. Channel boundary: bearer auth on `/v1`

The API deliberately has no principal authentication (the single-trusted-client
posture); today the socket is the credential. Production makes the one channel
an actual boundary: a static bearer token (`AURORA_API_TOKEN`, constant-time
compare) required on every `/v1` request, TLS terminated by a reverse proxy in
front, and a loopback/cluster-internal bind by default. Task resolution keeps
its own second factor — the per-task HMAC resolution token — unchanged. This is
one principal, not many: per-principal grants stay parked with multi-tenancy.
Lands in aurora-dist (`internal/dist/api` middleware + config).

### 27. HTTP server hardening

`http.Server` currently runs without timeouts. Add `ReadHeaderTimeout`,
`ReadTimeout`, `WriteTimeout`, `IdleTimeout`, `MaxHeaderBytes`, and request
body size limits on the POST endpoints; add `GET /healthz` for liveness (the
process-level check a supervisor needs — deeper health is the metrics' job).
Lands in aurora-dist (`cmd/aurora-dist`, `internal/dist/api`).

### 28. Durability ops: backup + tested restore

The SQLite file is the entire system state — the journal is the process, the
task, the audit trail. Treat it accordingly: verify WAL mode and
`busy_timeout` on open; continuous replication (Litestream) or a snapshot cron
(`VACUUM INTO`) including the `instance_id` file beside it; and a restore
drill that has actually been executed once, written into the runbook. Journal
growth — unbounded by deliberate choice since compaction was removed — gets a
gauge and an alert threshold plus a documented break-glass archive procedure;
a real lifecycle feature returns only when the gauge says so. Mostly docs and
deployment config; small verification bits in aurora-dist's store.

### 29. Operational metrics, alerts, runbook

The OTel exporter ships journal traces; nobody gets paged. Add operational
metrics beside it: terminal process counts by status *and cause*, executed and
failed compensations (a failed rollback is undefined external state — page),
re-drive wall hits, pending-task age, timer reconcile lag, DB size, LLM call
latency/errors. Alert set on: failed compensation, task nearing expiry
unresolved, DB size threshold. The runbook explains what a rollback report is
(the remediation map), how to resolve/deny tasks, retry/stop semantics, and
the program drain-and-deprecate procedure the digest law implies. Lands across
aurora-capcompute (counters at the seams) and aurora-dist (exposition).

### 30. LLM spend budget per process

Loops are bounded (abort-retry budget, re-drive wall, scheduler quotas);
money is not. A per-grant budget in the `openaillm` driver settings — max
calls and/or max tokens per process — enforced in the driver, exceeded budget
returned as a failed result with a clear errno the program can react to (wrap
up, abort, or ask). Lands in aurora-dispatchers (`openaillm`).

### 31. Deploy shape: single-writer posture, manifests, playbooks

Make the honest posture explicit and easy: **replicas=1**, documented — SQLite
is single-writer and the lease + stable-instance-id design is built for one
live writer; a second replica correctly blocks on leases rather than
corrupting anything. Ship a systemd unit and a k8s manifest (stateful volume
for the data dir, SIGTERM grace matching the runtime's Close timeout).
Programs arrive by CI dropping built `.wasm` into the programs directory; the
poller and the digest law do the rest. Secrets stay env-injected
(`AURORA_TASK_SECRET`, provider keys); rotation = restart. Docs plus two small
manifest files in aurora-dist.

## Out of scope (parked, with return triggers)

- **Multi-tenancy** — per-principal grants via attenuation tokens (macaroons,
  Birgisson et al., NDSS '14; the D3 policy layer), per-tenant ceilings and
  quotas, a silo-per-tenant control plane, metering folded from the journal.
  The design intent stays: `TenantID` already threads scopes, leases, memory,
  and task tokens, and the ceiling already speaks `sys.Attenuate`. Returns
  when a second tenant is real, as identity → policy → silo control plane, in
  that order.
- **MCP driver** — bridging stdio MCP servers as granted syscalls
  (`mcp.<server>.<tool>`). Dropped for now: it multiplies the external surface
  before the first deployment needs it, and its open-ended tool lists sit
  awkwardly under the capability ceiling. Returns with the first real
  deployment that needs a tool only MCP provides, behind explicit per-server
  tool lists.
- **Additional channels** (chat sources as inbound drivers, streaming APIs) —
  one channel is the boundary; a second channel is a second boundary and comes
  with the product need, not before.
- **Snapshot/checkpoint to bound replay cost** — replay cost is not a measured
  problem; the single-level-store move waits for evidence.
- **Journal lifecycle (retention/archival)** — removed once already as
  premature machinery; returns as a product/compliance feature when #28's
  growth gauge demands it.
- **Async spawn / pooled multi-tenant stores / scale-out placement** — density
  and concurrency optimizations with no current customer.

## Ledger — the OS model, built

| # | Item | Status |
|---|------|--------|
| 0 | Ambient-surface lockdown (kernel owns guest WASI sources) | **done** (`ambient.go`, `ErrAmbientAuthority`) |
| 1 | Journal program-versioning + replay compatibility check | **done** (`journaled.Header`, `ReplayIncompatibleError`) |
| 2 | Kernel-law CI tests (the five invariants as tests) | partial — laws 1/2 unit-tested; the rollback matrices and runtime tests cover the rest in practice |
| 3 | Hash-chained journal (tamper-evident audit) | **done** (`prev_hash`, `journaled.Verify`) |
| 4 | Capability attenuation helper in `sys` | **done** (`sys.Attenuate`) |
| 5 | `process.spawn` syscall (sync-first child processes) | **done** (`sys.spawn`, `spawn.go`) |
| 6 | ABI version field, errnos, savepoint syscalls | **done** (`sys.ABIVersion`, `sys.Errno`, `sys.SyscallBegin/Commit`) |
| 9 | Intent/completion journal records (journal-before-execute) | **done** (two-record tape, idempotency keys) |
| 10 | Guest-registered compensation + abort-retry | **done** (see §10 below — the full rollback semantics) |
| 11 | Information-flow labels + provenance (CaMeL-style) | **done** (labels, flow policy, `sys.declassify`) |
| 12 | Resource management (mem cap, resume deadline, quotas) | **done** (`MaxMemoryPages`/`ResumeTimeout`; `sched.Quota`) |
| 13 | Reference-monitor validation (grant-set + InputSchema) | **done** (`Validator`, `validate.go`) |
| 14 | Deterministic simulation testing harness | **done** (`sim/`, crash matrices) |
| 15 | Scheduler: priority, admission, virtual-actor activation | **done** (`sched/`) |
| 16 | Journal lifecycle: snapshot + compaction + retention | removed — built, then taken out as premature; see *Out of scope* |
| 17 | Journal→OpenTelemetry exporter | **done** (`otelexport/`) |
| 18 | Exactly-once effects: drivers honor idempotency keys | **done** (memory driver activity memory) |
| 19 | Reservation / TCC as a pattern | **done** — a pattern over dispatch + compensate, not a driver (see §19) |
| 20 | Approval-composable compensation (yielding inverse) | **done** (rollback parks on the inverse's task and resumes) |
| 21 | Deterministic rollback matrix | **done** (abort + guest-failure stories at every append position) |
| 22 | Journaled time & randomness (`sys.now`, `sys.random`) | **done** (worldDispatcher below replay) |
| 24 | Plan/execute program (CaMeL) | **done** (camel: quarantined planner, `$N` routing) |
| 25 | Attempt-scoped idempotency keys across rollbacks | **done** (records carry their writing revision; keys derive from the record) |

Items 7 (snapshot/checkpoint), 8 (chat sources as drivers), and 23
(macaroons) moved to *Out of scope* with their return triggers.

### 10. Guest-registered compensation + abort-retry

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
appends nothing has hit a deterministic wall; only then is the revision
**abandoned**: the registered compensations run, then the process reports
failed with its original error. A **stop** rolls back immediately — the
human asked for an end, not a resume — and an explicit **restart** abandons
the whole revision (top-level registrations included) before re-running from
scratch.

*Abandoning a revision* — deciding it can never run again — is the only
source of rollback, and a settled rollback is the only license to fork: at
the section's begin for a retry, at 0 for a restart. Forks are the only
events that mint revisions. The journal stays the guest's narrative
throughout: `sys.abort` appears on it only when the guest called it, and a
host abandonment (failure, stop, restart) is **management state** stamped
durably on the process — its only journal trace the compensation section it
appends, its conclusion the fork itself. The stamp standing past a terminal
conclusion is what licenses the retry of a zero-registration scope to fork:
that rollback leaves no journal trace at all.

### 19. Reservation / TCC as a pattern

Sagas have no isolation (García-Molina & Salem '87; Richardson's
countermeasures: semantic lock, pending state). The resolution: Try-Confirm/
Cancel needs **no kernel feature and no driver** — it is a usage pattern of
the primitives §10 already guarantees, because the pending state belongs to
the *participant*, not the orchestrator. In Pardon & Pautasso's RESTful TCC
the hold is a resource on the service that owns it; the coordinator only
remembers what to confirm or cancel — which is exactly what the journal and
`sys.compensate` already are. Aurora writes to third-party systems that every
reader treats as the source of truth: a reservation is only real if it is
written *there*. An orchestrator-side hold table (a `core.hold` driver
shipped briefly, then removed) is a reservation no other booker can see, and
an orchestrator-imposed TTL is the resource owner's policy usurped.

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

### 25. Attempt-scoped idempotency keys across rollback boundaries

Crash re-drive (same attempt: the key must be stable) and rollback retry (a
new transaction: the key must be fresh) are different beasts, and a key of
bare `(header, position, call-hash)` dressed them the same: a retry that
reproduced byte-identical args at the same position would *adopt* the
rolled-back attempt's recorded effect — an effect its rollback had already
compensated. Resolved within the existing abstractions: the revision **is**
the attempt identity. Each record carries the revision that first wrote it
(stamped by the Journal at append), and intent identity derives from the
record — `(header, revision, position, call-hash)`. A re-driven open intent
sits in the fork's shared prefix with its origin revision intact, so it
recomputes its original key however many resume forks intervene; a
rolled-back section's retry re-executes live under the new revision and gets
a fresh key space. Uniform for children — their journals scope their own
keys. Proven by the crash matrices and the recharge test.
