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

- `Config`, `Program`, `NewProgram` — compiling a program image
- `Process`, `ProcessSpec`, `NewProcess` — instantiating a process
- `Resume`, `ResumeResult` — one cooperative quantum
- the ambient lockdown (`ambient.go`) and the syscall host function (`host.go`)

Do not add root packages, public packages, examples, or old engine concepts unless
explicitly asked.

## Ownership Rules

Parent packages own interfaces and vocabulary.

Child packages own concrete implementations.

Current boundaries:

```text
capcompute
  program images, processes, the Resume quantum, the host function

sys
  Dispatcher interface
  Syscall, SyscallResult, Errno
  Authorization
  the reserved sys.* syscall names
```

The journal, replay, the reference monitor, scheduling, and process lookup are
not here. They live in aurora-capcompute (`replay/`, `journaled/`, `monitor/`,
`internal/agent/`).

If a type appears in an interface method, it belongs with that interface unless
there is a stronger owner.

## Import Direction

Dependencies go downward or sideways to parent boundaries.

Allowed:

```text
capcompute -> sys
```

If an import cycle appears, fix ownership. Do not add glue packages to hide it.

## Process Model

`Program` owns one compiled image. Many processes may run one program.

`Process` owns:

- the host-side credential (`Cred`)
- its input and entrypoint
- the reusable Extism plugin instance
- the dispatcher it was created with

`Resume` plants a dispatch closure, already bound to the process's credential
and dispatcher, in the call context. The host function serves exactly the
process that holds the CPU and needs no lookup.

A stopped process is terminated permanently. The wrapping system decides when
to create, resume, and close processes.

## Replay Model

Guest code re-enters from the top.

Replay is another invocation of a fresh process with the same input and a
dispatcher that serves recorded results. The processor does not know it is
replaying. Do not put replay, async completion, or journal-writing APIs here.

## Package Names

Names must read well at call sites.

Prefer concrete strategy names:

```go
replay.Dispatcher
journaled.Tape
monitor.Validator
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
