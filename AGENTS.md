# AGENTS.md

This repo is the experimental Extism compute runtime.

Write simple Go. Put code where ownership is obvious.

Decide in this order:

```text
ownership -> visibility -> package -> file
```

## Current Shape

The root package is the library entrypoint.

Root `capcompute` owns:

- `ComputeCompiledPlugin`
- `Config`
- `Session`
- `SessionKey`
- `SessionStore`
- `SessionRecord`
- `PlayRequest`
- `PlayResult`
- session lifecycle
- Extism plugin creation and host callback wiring

Do not add root packages, public packages, examples, or old engine concepts unless
explicitly asked.

## Ownership Rules

Parent packages own interfaces and vocabulary.

Child packages own concrete implementations.

Current boundaries:

```text
capcompute
  compiled plugin, sessions, SessionStore, Play/Replay lifecycle

dispatcher
  Dispatcher interface
  DispatcherFactory interface
  Call
  Outcome

dispatcher/replay
  replay Dispatcher decorator
  Tape interface

dispatcher/replay/tape/journaled
  journal-backed Tape implementation
  Journal interface

dispatcher/replay/tape/journaled/journal/memory
  in-memory Journal implementation
```

If a type appears in an interface method, it belongs with that interface unless
there is a stronger owner.

## Import Direction

Dependencies go downward or sideways to parent boundaries.

Allowed:

```text
capcompute -> dispatcher
capcompute -> dispatcher/replay
dispatcher/replay -> dispatcher
journaled -> dispatcher
```

Avoid:

```text
child package -> capcompute
implementation package -> sibling implementation package
```

If an import cycle appears, fix ownership. Do not add glue packages to hide it.

## Session Model

`ComputeCompiledPlugin` owns the session map and active-session exclusivity.
`SessionStore` is the root-owned persistence boundary for yielded session
records.

`Session` owns:

- guest data
- original `PlayRequest`
- reusable Extism plugin instance
- current dispatcher chain
- yielded call

Context passed into Extism host callbacks carries only the session id.

Yielded sessions are retained for replay.
Completed or failed sessions are finalized and removed.

If a `SessionStore` is configured:

- yielded sessions are saved;
- completed or failed sessions are deleted;
- concrete persistent implementations stay outside the root package.

## Replay Model

Guest code re-enters from the top.

Replay is another invocation of the same session:

- `Play` creates a dispatcher chain.
- `Yield` keeps that dispatcher chain in the session.
- async completion is handler/journal responsibility.
- `Replay(ctx, sessionID)` reuses session guest data, request, and dispatcher.

Do not put async completion or journal-writing APIs on `ComputeCompiledPlugin`.

Replay dispatcher behavior:

- replay from tape when a record exists;
- delegate upstream when no record exists;
- record deterministic `OutcomeResult` and `OutcomeFailed`;
- reset tape on `OutcomeYield`;
- do not record `OutcomeYield`.

## Package Names

Names must read well at call sites.

Prefer concrete strategy names:

```go
replay.Dispatcher
journaled.Tape
memory.Journal
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
