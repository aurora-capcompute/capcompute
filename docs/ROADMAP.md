# Roadmap

The OS model exists (the ledger at the end lists it, and `ARCHITECTURE.md`
describes the model and its laws). The current goal is production in the narrowest posture:
**one tenant, and one channel, the `/v1` HTTP API, as the trust boundary**. One
organization, one runtime instance, one SQLite file. Multi-tenancy, more
channels and scale-out are parked until a single tenant runs in production.

Score = impact (H/M/L) × effort (S/M/L). Each item can land alone.

## Now: production, one tenant, one channel

| # | Item | Impact | Effort | Status |
|---|------|--------|--------|--------|
| 26 | Bearer auth on `/v1` | H | S | open |
| 27 | HTTP server timeouts | M | S | open |
| 28 | Backup and a tested restore | H | S–M | open |
| 29 | Metrics, alerts, runbook | H | M | open |
| 30 | LLM spend budget per process | M–H | S | open |
| 31 | Deploy shape: one writer, manifests, playbooks | M | S | open |

### 26. Bearer auth on `/v1`

The API has no principal authentication today: the socket is the credential.
Require a static bearer token (`AURORA_API_TOKEN`, constant-time compare) on
every `/v1` request, terminate TLS in a reverse proxy, and bind to loopback or a
cluster-internal address by default. Task resolution keeps its own per-task
HMAC token. This is one principal. Per-principal grants stay parked with
multi-tenancy. Lands in aurora-dist (`internal/dist/api`).

### 27. HTTP server timeouts

`http.Server` sets no timeouts. Add `ReadHeaderTimeout`, `ReadTimeout`,
`WriteTimeout`, `IdleTimeout` and `MaxHeaderBytes`. Request bodies are already
capped at 4 MiB, and `GET /healthz` already exists. Lands in aurora-dist
(`cmd/aurora-dist`).

### 28. Backup and a tested restore

The SQLite file is the whole system state. The store already opens it with WAL,
`busy_timeout` and `synchronous=FULL`. Add continuous replication (Litestream)
or a snapshot job (`VACUUM INTO`), and run one restore drill and write it into
the runbook. The journal grows without bound. Add a size gauge, an alert
threshold and a documented archive procedure. A retention feature returns only
when the gauge requires it.

### 29. Metrics, alerts, runbook

The server exports no metrics today. Add the metrics that page: counts of
finished processes by status and cause, abort-retry budget exhaustion, re-drive
walls, pending task age, timer fire lag, DB size, LLM latency and errors. Alert on tasks near
expiry and on DB size. The runbook covers task resolution, retry and stop, and
the program drain procedure that the digest binding requires. Lands across
aurora-capcompute (counters) and aurora-dist (exposition).

### 30. LLM spend budget per process

The abort-retry budget and the re-drive wall bound loops. Nothing bounds spend.
Add a per-grant budget to the `openaillm` settings (max calls or max tokens per
process). The driver enforces it and returns a failed result with a clear errno,
so the program can wrap up or abort. Lands in aurora-dispatchers (`openaillm`).

### 31. Deploy shape

State the posture: **one replica**. SQLite has one writer, and the runtime
folds the log into memory once, at boot, so one runtime owns one store. Ship a
systemd unit and a Kubernetes manifest (a persistent volume for the data
directory, a SIGTERM grace period that matches the runtime's close timeout).
Run the server as a dedicated user with least-privilege credentials, and limit
egress in the platform: this is where `core.command` is confined. Programs
arrive when CI drops built `.wasm` files into the programs directory and the
server restarts. Secrets come from the environment, and rotation is a restart.

## Out of scope (parked, with return triggers)

- **Multi-tenancy:** per-principal grants (attenuation tokens, macaroons), a
  policy layer, per-tenant quotas, a silo per tenant, metering from the journal.
  Returns when a second tenant is real.
- **Per-command OS sandbox** for `core.command`: bubblewrap or Landlock on
  Linux, `sandbox-exec` on macOS, with read, write and network policy per
  command, and fail closed. Today the author vouches for each command, and the deployment
  confines the server. Returns when one of these is real:
  - a command runs code that the model wrote, or takes code or file paths as a
    slot (run such code as a Wasm guest instead, if possible).
  - more than one tenant shares a server.
  - commands in one deployment need different rights.
- **MCP driver** (`mcp.<server>.<tool>`). Its open-ended tool lists do not fit
  a static grant set. Returns when a deployment needs a tool that only MCP
  provides, with explicit per-server tool lists.
- **More channels** (more inbound connectors, streaming APIs). A second channel
  is a second boundary. Returns with the product need.
- **Snapshots** to bound replay cost. Returns when replay cost is measured as a
  problem.
- **Journal retention and archival.** Returns when #28's gauge requires it.
- **Async spawn, pooled stores, scale-out placement.** No current customer.

## Ledger: the OS model

Git history keeps the design of every removed item.

| # | Item | Status |
|---|------|--------|
| 0 | Ambient-surface lockdown | done (`ambient.go`, `ErrAmbientAuthority`) |
| 1 | Program versioning for replay | done as a digest binding: a process never runs under a changed program. The journal header was removed. |
| 2 | Laws as tests | partial: laws 1 and 2 have unit tests. The runtime tests cover the rest |
| 3 | Hash-chained journal | removed. `journaled.Verify` checks structure only |
| 4 | Capability attenuation helper | removed, as was the dist's capability ceiling |
| 5 | `spawn` (child processes) | removed. See ARCHITECTURE, *Parked designs* |
| 6 | ABI version, errnos, savepoint syscalls | done (`sys.ABIVersion`, `sys.Errno`, `sys.begin`/`sys.commit`) |
| 9 | Intent/completion records | done. Idempotency keys for drivers were removed |
| 10 | Compensation and abort-retry | abort-retry done (the section runs again). Compensation removed |
| 11 | Information-flow labels and provenance | done (labels, flow policy, `sys.declassify`) |
| 12 | Resource management | done (`MaxMemoryPages`, `ResumeTimeout`). Quotas and the `Throttle` limiter removed |
| 13 | Reference-monitor validation | done (`monitor.Validator`) |
| 14 | Deterministic simulation tests | done (aurora-capcompute `sim/`) |
| 15 | Scheduler with priority and admission | removed. The scheduler is a worker pool (`internal/agent/pool.go`) |
| 16 | Snapshots, compaction, retention | removed. Parked |
| 17 | Journal → OpenTelemetry exporter | removed. Returns with #29 |
| 18 | Exactly-once effects | removed. Effects are at least once in the crash window |
| 19 | Reservation (TCC) pattern | removed with compensation |
| 20 | Approval-composable compensation | removed with compensation |
| 21 | Rollback matrix | removed with compensation. Abort and re-run tests remain |
| 22 | Journaled time and randomness | done (`sys.now`, `sys.random`) |
| 24 | Plan/execute program (CaMeL) | removed (the `camel` program) |
| 25 | Attempt-scoped idempotency keys | removed with the keys. Records still carry their revision |
