# Scope

The process-reconstruction primitives the library set out to provide are in
place: `ProcessTable` is interface-only (`LoadProcess` / `SaveProcess`),
`CreateProcess` rebuilds a process and its dispatcher chain from a
`ProcessSpec`, and the syscall host function reloads the process from the table
on each guest syscall (see `host.go` and the package doc in `doc.go`).
Reconstructing a yielded process after a restart is therefore `CreateProcess` +
`SaveProcess` under the application's control — the library deliberately
exposes no `Replay(pid)` entry point, because *when* a process resumes and
*what* is injected back are the wrapping system's decisions.

This library deliberately does not own, and will not grow:

- concrete durable store implementations;
- replay, journals, or async completion;
- reference-monitor policy (grants, schemas, flow labels);
- dispatching syscalls to other guests;
- schedulers, engines, or product-specific workflow code.

Those belong to the systems built on top of `capcompute` — the monitor,
replay, and journal layers live in aurora-capcompute (`monitor/`, `replay/`,
`journaled/`) since the 2026-07-19 charter passes. capcompute is the
processor: it runs programs deterministically and hands every syscall to the
`sys.Dispatcher` the process was created with; what that dispatcher enforces
is the layer above's business.

The rule is visible in the tree: every `.go` file here is either consumed
kernel API or a `_test.go` file. Built-ahead code with no consumer gets
removed — the IPC razor — with its design kept in docs until a consumer
forces it back.

For scored next steps, see `docs/ROADMAP.md`.
