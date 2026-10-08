# Scope

The processor is complete for its job:

- `NewProgram` compiles an image.
- `NewProcess` creates a process from a `ProcessSpec`.
- `Resume` runs one quantum. The host function dispatches through the closure
  that `Resume` binds (see `host.go` and `doc.go`).

The library has no `Replay(pid)` call. The wrapping system decides when a
process resumes, and what it gets back.

This library does not own, and will not grow:

- durable stores.
- replay, journals or async completion.
- reference-monitor policy (grants, schemas, flow labels).
- dispatch to other guests.
- schedulers or product workflow code.

aurora-capcompute owns those (`monitor/`, `replay/`, `journaled/`,
`internal/agent/`). Every `.go` file here is processor API that something uses,
or a test. Code with no consumer is removed.

Next steps: `docs/ROADMAP.md`.
