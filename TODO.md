# Scope

The session-reconstruction primitives the library set out to provide are in
place: `SessionStore` is interface-only (`LoadSession` / `SaveSession`),
`CreateSession` rebuilds a session and its dispatcher chain from a
`PlayRequest`, and host callbacks reload the session from the store on each
guest call (see `host.go` and the package doc in `doc.go`). Reconstructing a
yielded session after a restart is therefore `CreateSession` + `SaveSession`
under the application's control — the library deliberately exposes no
`Replay(sessionID)` entry point, because *when* a session resumes and *what*
is injected back are the wrapping system's decisions.

This library deliberately does not own, and will not grow:

- concrete persistent store implementations;
- replay scheduling, queues, or async completion;
- dispatching calls to other guests;
- schedulers, engines, or product-specific workflow code.

Those belong to the systems built on top of `capcompute`.
