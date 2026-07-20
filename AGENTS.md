# AGENTS.md

This repo is the experimental Extism compute runtime — the processor of a small
library operating system (see `docs/ARCHITECTURE.md` for the OS model and its
invariants).

Write simple Go. Put code where ownership is obvious.

Decide in this order:

```text
ownership -> visibility -> package -> file
```

## Current Shape

The root package is the library entrypoint.

Root `capcompute` owns:

- `Kernel`
- `Config`
- `Process`
- `PID`
- `ProcessTable`
- `ProcessSpec`
- `ResumeResult`
- process lifecycle
- Extism plugin creation and syscall host-function wiring

Do not add root packages, public packages, examples, or old engine concepts unless
explicitly asked.

## Ownership Rules

Parent packages own interfaces and vocabulary.

Child packages own concrete implementations.

Current boundaries:

```text
capcompute
  Kernel (compiled program image), processes, ProcessTable, Resume/Replay lifecycle

sys
  Dispatcher interface
  Syscall
  SyscallResult
  Capability, Authorization

sys/replay
  replay Dispatcher decorator
  Tape interface

sys/replay/tape/journaled
  journal-backed Tape implementation
  Journal interface

memory
  in-memory ProcessTable implementation
```

If a type appears in an interface method, it belongs with that interface unless
there is a stronger owner.

## Import Direction

Dependencies go downward or sideways to parent boundaries.

Allowed:

```text
capcompute -> sys
capcompute -> sys/replay
sys/replay -> sys
journaled -> sys
memory -> capcompute
```

If an import cycle appears, fix ownership. Do not add glue packages to hide it.

## Process Model

`Kernel` owns compiled-program instantiation and active-process exclusivity.
`ProcessTable` is the root-owned lookup boundary for live processes.

`Process` owns:

- guest data
- original `ProcessSpec` input
- reusable Extism plugin instance
- current dispatcher chain

Context passed into the syscall host function carries only the PID.

Yielded processes are retained for replay.
Completed or failed processes are finalized and removed by the wrapping system.

## Replay Model

Guest code re-enters from the top.

Replay is another invocation of the same process:

- `Resume` runs the process against its dispatcher chain.
- Yield keeps that dispatcher chain in the process.
- async completion is handler/journal responsibility.

Do not put async completion or journal-writing APIs on `Kernel`.

Replay dispatcher behavior:

- replay from tape when a record exists;
- delegate upstream when no record exists;
- record deterministic `StatusResult` and `StatusFailed`;
- reset tape on `StatusYield`;
- do not record `StatusYield`.

## Package Names

Names must read well at call sites.

Prefer concrete strategy names:

```go
replay.Dispatcher
journaled.Tape
memory.ProcessTable
```

Avoid:

```text
common
utils
models
helpers
impl
manager
service
```

## Interfaces

Create interfaces only for real boundaries:

- dispatcher chains;
- tape/replay storage;
- handler execution;
- external I/O or test substitution.

Keep interfaces small.

## Tests

Put tests next to the package they verify.

Child package tests must not import parent `capcompute` just for convenience.
Use the owning package vocabulary directly.

Always run:

```sh
go test ./...
go vet ./...
```

## Final Rule

Keep code local, boring, and ownership-driven.

Do not create files, packages, or interfaces until the owner is clear.
