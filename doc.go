// Package capcompute is the root API for running Extism compute guests with
// host-dispatched capabilities.
//
// The package owns compiled plugin lifecycle, session lifecycle, and the
// guest-to-host callback wiring. Concrete storage, durable reconstruction, and
// application scheduling stay outside this package.
//
// A typical runtime does the following:
//   - build a ComputeCompiledPlugin with a wasm Manifest, a DispatcherFactory,
//     and a SessionStore;
//   - create a Session from a PlayRequest;
//   - save that Session in the SessionStore before invoking Play if the guest
//     can call host capabilities;
//   - call Play and read the single PlayResult from the returned handle;
//   - close sessions and the compiled plugin at the application boundary.
//
// Host callbacks receive only the session id through context. The host function
// loads the Session from SessionStore and dispatches the guest Call through the
// session dispatcher. This keeps runtime lookup explicit and avoids hidden play
// state in context.
//
// Play has four observable outcomes. A successful guest call whose JSON output
// contains {"status":"yielded"} returns PlayYielded, while an explicit
// {"status":"completed"} returns PlayCompleted. Missing or unsupported status
// values return PlayFailed. A stopped invocation returns PlayStopped and
// permanently terminates that physical session. Guest and runtime errors also
// return PlayFailed.
//
// SessionStore is a runtime lookup boundary. Durable stores should persist the
// data needed by their application to recreate sessions, then hydrate a fresh
// ComputeCompiledPlugin with CreateSession and SaveSession when a process
// restarts. CreateSession deliberately does not save sessions; callers decide
// when a session becomes visible to host callbacks and when it is removed.
//
// The library does not own replay scheduling, async completion, durable journal
// policy, or session cleanup timing. Those are conventions of the wrapping
// system using this package.
package capcompute
