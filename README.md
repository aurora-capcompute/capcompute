# capcompute

`capcompute` is an experimental Extism-based compute runtime library.

It gives a Go application a small, explicit way to run WebAssembly guests that
can call back into host-owned capabilities. The library owns the Extism plugin
lifecycle and the host callback wiring. The application owns scheduling,
persistence policy, replay policy, queues, durable state, and cleanup timing.

The intended use case is a higher-level runtime for systems such as AI agents,
controllers, and workflow-like applications where guest code can request host
work, yield, and later be invoked again by the wrapping system.

## What This Library Owns

The root `capcompute` package owns:

- compiled Extism plugin creation;
- per-session Extism plugin instances;
- the `Session`, `SessionKey`, and `SessionStore` vocabulary;
- the `PlayRequest` and `PlayResult` lifecycle;
- the `extism:host/compute/play` host callback;
- lookup of the current session from the configured `SessionStore`;
- dispatching guest host calls into a `dispatcher.Dispatcher`.

Child packages own concrete implementations and optional strategies:

- `dispatcher` defines `Call`, `Outcome`, `Dispatcher`, and `DispatcherFactory`;
- `dispatcher/replay` provides a replay dispatcher decorator;
- `dispatcher/replay/tape/journaled` provides a journal-backed replay tape;
- `session_store_memory` provides an in-memory `SessionStore`.

## What This Library Does Not Own

`capcompute` deliberately does not own:

- job queues or schedulers;
- async completion;
- when a yielded session should resume;
- what value is injected back into guest logic after outside work completes;
- durable database implementations;
- when sessions are saved, deleted, or closed;
- product-specific workflow, agent, or controller policy;
- plugin distribution or marketplace concerns.

Those belong to the system wrapping this library.

## Runtime Flow

A host application usually does this:

1. Create a `ComputeCompiledPlugin` with an Extism manifest, a dispatcher
   factory, and a session store.
2. Create a `Session` from a `PlayRequest`.
3. Save the session in the `SessionStore` before calling `Play` if the guest can
   call host capabilities.
4. Call `Play`.
5. Read the single `PlayResult` from the returned handle's result channel.
6. Decide whether to save, keep, delete, recreate, or close the session.

`CreateSession` does not save anything. This is intentional. The caller decides
when a session becomes visible to host callbacks.

`Play` invokes the configured entrypoint on the session's reusable Extism plugin
instance. It returns a `PlayHandle` because execution happens in a goroutine.
Each handle sends exactly one `PlayResult` and then closes its result channel.
Calling `Stop` interrupts the invocation and permanently terminates that physical
session instance.

Calling `Play` again on a stopped session returns `ErrSessionTerminated`.
Recreate the logical run with `CreateSession`, the same request and session key,
then replace the old session in the store. A replay dispatcher can reuse
previously committed outcomes from the same journal. The capability call that
was active during cancellation may execute again because it had no committed
outcome.

Journal-backed replay records deterministic result and failed outcomes. Yield
is never committed. Dispatchers should return a Go error for cancellation or
other transient infrastructure failures that must be retried rather than
returning a deterministic failed outcome.

## Session Model

`Session` is the runtime object for one logical run. It contains:

- user-owned guest data;
- the original JSON input;
- the entrypoint to call;
- the Extism plugin instance;
- the dispatcher chain for host calls.

`Session` is not thread-safe. A wrapping runtime should coordinate concurrent
use of the same session.

`SessionKey` is implemented by user data:

```go
type Run struct {
	ID string
}

func (r Run) SessionKey() string {
	return r.ID
}
```

The session key is the only tracking value carried through callback context.
Host callbacks use it to load the current session from `SessionStore`.

## SessionStore

`SessionStore` is the runtime lookup boundary:

```go
type SessionStore[ID comparable, K SessionKey[ID]] interface {
	LoadSession(ctx context.Context, sessionID ID) (*Session[K], error)
	SaveSession(ctx context.Context, sessionID ID, session *Session[K]) error
}
```

The store owns session visibility. If a guest calls the host callback and the
session is not in the store, the callback returns a failed host response.

The in-memory implementation lives in `session_store_memory`. Durable stores
should live outside the root package. A durable store can persist the
application data needed to recreate sessions, then hydrate a new
`ComputeCompiledPlugin` after process restart by calling `CreateSession` and
`SaveSession` for each persistent session.

## Host Callback Contract

Guests call this imported function:

```go
//go:wasmimport extism:host/compute play
func hostPlay(uint64) uint64
```

The argument points to JSON matching `dispatcher.Call`:

```json
{
  "name": "tool.name",
  "args": {"any": "json"}
}
```

The host callback:

1. reads the session id from context;
2. loads the session from `SessionStore`;
3. decodes the `dispatcher.Call`;
4. calls the session dispatcher;
5. returns a JSON host response to the guest.

The host response has this shape:

```json
{
  "status": "result",
  "result": {"any": "json"}
}
```

`status` is one of:

- `result`: the host call completed and returned JSON;
- `yield`: the host needs outside work before the guest can make progress;
- `failed`: the host call failed.

The guest decides what to do with that response. In particular, a host
`yield` outcome does not automatically pause the guest. The guest must return
from its exported function with the play-result convention described below.

## Play Result Convention

`PlayResult.Status` is derived from the Extism call result:

- `PlayFailed`: the guest call returned an Extism/runtime error;
- `PlayStopped`: the play context was cancelled and the physical session was
  terminated;
- `PlayYielded`: the guest succeeded and returned JSON with
  `{"status":"yielded"}`;
- `PlayCompleted`: the guest succeeded and returned JSON with
  `{"status":"completed"}`;
- `PlayFailed`: the guest output was not valid JSON or did not contain one of
  those two explicit statuses.

This convention keeps the library minimal. Yielding only means "this play
attempt stopped at a point chosen by the guest." The wrapping system decides
when to invoke the session again and what data should be available when it does.

## Minimal Host Setup

```go
ctx := context.Background()
store := session_store_memory.New[string, Run]()

compute, err := capcompute.NewComputeCompiledPlugin[string, Run](ctx, capcompute.Config[string, Run]{
	Manifest: extism.Manifest{
		Wasm: []extism.Wasm{extism.WasmFile{Path: "plugin.wasm"}},
	},
	PluginConfig: extism.PluginConfig{
		EnableWasi: true,
	},
	Dispatchers:  dispatcherFactory{},
	SessionStore: store,
})
if err != nil {
	return err
}
defer compute.CloseCompiled(ctx)

run := Run{ID: "run-1"}
session, err := compute.CreateSession(ctx, capcompute.PlayRequest[string, Run]{
	Input:      json.RawMessage(`{"task":"example"}`),
	Entrypoint: "run",
	UserData:   run,
})
if err != nil {
	return err
}
defer session.Close(ctx)

if err := store.SaveSession(ctx, run.SessionKey(), session); err != nil {
	return err
}

handle, err := compute.Play(ctx, session)
if err != nil {
	return err
}

result := <-handle.Results()
switch result.Status {
case capcompute.PlayCompleted:
	// The guest finished this play attempt.
case capcompute.PlayYielded:
	// Keep or persist enough state for the wrapping system to resume later.
case capcompute.PlayStopped:
	// Recreate the session before replaying this logical run.
case capcompute.PlayFailed:
	// Inspect result.Err and apply application error policy.
}
```

The dispatcher factory is application code. It builds a dispatcher chain for one
session:

```go
type dispatcherFactory struct{}

func (dispatcherFactory) NewDispatcher(context.Context, Run) (dispatcher.Dispatcher[Run], error) {
	return runDispatcher{}, nil
}

type runDispatcher struct{}

func (runDispatcher) Dispatch(ctx context.Context, run Run, call dispatcher.Call) (dispatcher.Outcome, error) {
	switch call.Name {
	case "echo":
		return dispatcher.Result(call.Args), nil
	case "wait":
		return dispatcher.Yield("waiting for outside work"), nil
	default:
		return dispatcher.Failed("unknown call"), nil
	}
}
```

Dispatchers may optionally implement `dispatcher.CapabilityProvider` to expose
guest-callable operation metadata. `dispatcher.WithCapabilities` decorates an
existing dispatcher without changing its dispatch behavior. Replay dispatchers
forward capability metadata from their wrapped dispatcher, and
`Session.Capabilities()` returns the capabilities of the exact session chain.

## Guest Convention

Guests are normal Extism plugins. A Go/TinyGo guest can use
`github.com/extism/go-pdk`.

The guest imports `extism:host/compute/play`, sends `dispatcher.Call` JSON, reads
the host response, and then either continues, returns a completed output, returns
`{"status":"yielded"}`, or sets an Extism error.

The integration fixture in `testdata/integration_guest` shows the smallest Go
guest that exercises completed, yielded, and failed play states.

## Replay

Replay is modeled as dispatcher behavior, not as a separate root lifecycle
method. Guest code re-enters from the top. A replay dispatcher can serve
recorded outcomes from a tape and delegate new calls upstream when the tape does
not contain a matching record.

The root package does not decide when replay happens or when async work is
complete. A wrapping runtime can build that policy by choosing the dispatcher
chain it creates for a session.

## Testing

Always run:

```sh
go test ./...
go vet ./...
```

There is an optional TinyGo integration test. If `tinygo` is installed, it builds
and runs the real guest fixture. If `tinygo` is not installed, the test skips.

In sandboxed environments where the default Go cache is not writable, use a
writable cache:

```sh
GOCACHE=/tmp/capcompute-go-build go test ./...
GOCACHE=/tmp/capcompute-go-build go vet ./...
```

## Extism Tooling

Extism also provides CLI and PDK tooling. The `extism` CLI can generate plugin
projects and call simple plugins. That is useful for plugin authors, but it does
not replace this library's integration tests because `capcompute` relies on a
custom host import that must be provided by Go test code.

XTP and schema-driven bindgen may be useful later if the guest/host contract is
published as a stable plugin-author API. For now, the library keeps that
contract explicit in Go types and JSON conventions.
