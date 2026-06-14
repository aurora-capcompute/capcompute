package capcompute

import (
	"capcompute/dispatcher"
	"context"
	"encoding/json"
	"errors"

	extism "github.com/extism/go-sdk"
)

var (
	ErrDispatcherRequired   = errors.New("dispatcher is required")
	ErrSessionRequired      = errors.New("session is required")
	ErrSessionActive        = errors.New("session is already playing")
	ErrSessionStoreRequired = errors.New("session store is required")
)

// SessionKey lets user-owned run data expose the stable identity used for session maps.
type SessionKey[ID comparable] interface {
	SessionKey() ID
}

// Config contains everything needed to compile a module and create per-run instances.
type Config[ID comparable, K SessionKey[ID]] struct {
	Manifest       extism.Manifest
	PluginConfig   extism.PluginConfig
	InstanceConfig extism.PluginInstanceConfig
	Dispatchers    dispatcher.DispatcherFactory[K]
	SessionStore   SessionStore[ID, K]
}

// ComputeCompiledPlugin owns one compiled module, dispatcher factory, and per-key sessions.
type ComputeCompiledPlugin[ID comparable, K SessionKey[ID]] struct {
	compiled       *extism.CompiledPlugin
	dispatchers    dispatcher.DispatcherFactory[K]
	sessionStore   SessionStore[ID, K]
	instanceConfig extism.PluginInstanceConfig
}

// Session owns the reusable Extism plugin instance for one key.
// Session state is not thread-safe; ComputeCompiledPlugin serializes Play per key.
type Session[K any] struct {
	GuestData  K
	Input      json.RawMessage
	Entrypoint string
	plugin     *extism.Plugin
	dispatcher dispatcher.Dispatcher[K]
}

// Close releases the Extism plugin instance owned by this session.
func (session *Session[K]) Close(ctx context.Context) error {
	return session.plugin.Close(ctx)
}

// SessionStore owns per-key sessions and their active/idle lifecycle.
type SessionStore[ID comparable, K SessionKey[ID]] interface {
	LoadSession(ctx context.Context, sessionID ID) (*Session[K], error)
	SaveSession(ctx context.Context, sessionID ID, session *Session[K]) error
	BeginSession(ctx context.Context, sessionID ID) error
	EndSession(ctx context.Context, sessionID ID) error
}

// NewComputeCompiledPlugin compiles a module and registers the dispatcher host function.
func NewComputeCompiledPlugin[ID comparable, K SessionKey[ID]](ctx context.Context, config Config[ID, K]) (*ComputeCompiledPlugin[ID, K], error) {
	if config.Dispatchers == nil {
		return nil, ErrDispatcherRequired
	}
	if config.SessionStore == nil {
		return nil, ErrSessionStoreRequired
	}

	compute := &ComputeCompiledPlugin[ID, K]{
		dispatchers:    config.Dispatchers,
		sessionStore:   config.SessionStore,
		instanceConfig: config.InstanceConfig,
	}

	compiled, err := extism.NewCompiledPlugin(ctx, config.Manifest, config.PluginConfig, []extism.HostFunction{
		hostFunction(compute.sessionStore),
	})
	if err != nil {
		return nil, err
	}
	compute.compiled = compiled
	return compute, nil
}

// PlayRequest is one guest invocation attempt.
type PlayRequest[ID comparable, K SessionKey[ID]] struct {
	Input      json.RawMessage
	Entrypoint string
	UserData   K
}

// PlayStatus is the result of one guest invocation attempt.
type PlayStatus string

const (
	PlayCompleted PlayStatus = "completed"
	PlayYielded   PlayStatus = "yielded"
	PlayFailed    PlayStatus = "failed"
)

// PlayResult is delivered when the play goroutine exits.
type PlayResult[K any] struct {
	Status PlayStatus
	Output json.RawMessage
	Exit   uint32
	Err    error
}

func (c *ComputeCompiledPlugin[ID, K]) CreateSession(ctx context.Context, request PlayRequest[ID, K]) (*Session[K], error) {
	plugin, err := c.compiled.Instance(ctx, c.instanceConfig)
	if err != nil {
		return nil, err
	}
	newDispatcher, err := c.dispatchers.NewDispatcher(ctx, request.UserData)
	if err != nil {
		return nil, err
	}

	return &Session[K]{
		GuestData:  request.UserData,
		Input:      request.Input,
		Entrypoint: request.Entrypoint,
		plugin:     plugin,
		dispatcher: newDispatcher,
	}, nil
}

// Play starts one exclusive guest invocation for key in its own goroutine.
func (c *ComputeCompiledPlugin[ID, K]) Play(ctx context.Context, session Session[K]) (<-chan PlayResult[K], error) {
	sessionKey := session.GuestData.SessionKey()
	results := make(chan PlayResult[K], 1)
	go func() {
		defer close(results)

		callCtx := context.WithValue(ctx, sessionKeyContextKey{}, sessionKey)

		exit, output, err := session.plugin.CallWithContext(callCtx, session.Entrypoint, session.Input)
		if err != nil {
			results <- PlayResult[K]{Status: PlayFailed, Exit: exit, Err: err}
		}
		status := playStatus(output)
		if status == PlayYielded {
			results <- PlayResult[K]{
				Status: PlayYielded,
				Output: append(json.RawMessage(nil), output...),
				Exit:   exit,
			}
		}
		results <- PlayResult[K]{
			Status: PlayCompleted,
			Output: append(json.RawMessage(nil), output...),
			Exit:   exit,
		}
	}()
	return results, nil
}

func playStatus(output []byte) PlayStatus {
	var envelope struct {
		Status PlayStatus `json:"status"`
	}
	if err := json.Unmarshal(output, &envelope); err != nil {
		return PlayCompleted
	}
	if envelope.Status == PlayYielded {
		return PlayYielded
	}
	return PlayCompleted
}
