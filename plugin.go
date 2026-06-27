package capcompute

import (
	"github.com/aurora-capcompute/capcompute/dispatcher"
	"context"
	"encoding/json"
	"errors"
	"sync"

	extism "github.com/extism/go-sdk"
	"github.com/tetratelabs/wazero"
)

var (
	ErrInvalidGuestOutput   = errors.New("invalid guest output")
	ErrSessionActive        = errors.New("session is already active")
	ErrSessionRequired      = errors.New("session is required")
	ErrSessionStoreRequired = errors.New("session store is required")
	ErrSessionTerminated    = errors.New("session is terminated")
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
	SessionStore   SessionStore[ID, K]
}

// ComputeCompiledPlugin owns one compiled module.
type ComputeCompiledPlugin[ID comparable, K SessionKey[ID]] struct {
	compiled       *extism.CompiledPlugin
	sessionStore   SessionStore[ID, K]
	instanceConfig extism.PluginInstanceConfig
}

// Session owns the reusable Extism plugin instance for one key.
// Session state is not thread-safe; callers coordinate concurrent use.
type Session[K any] struct {
	GuestData  K
	Input      json.RawMessage
	Entrypoint string
	plugin     *extism.Plugin
	dispatcher dispatcher.Dispatcher[K]
	state      sessionState
}

// Capabilities returns the operations exposed by this session's dispatcher.
func (session *Session[K]) Capabilities() []dispatcher.Capability {
	if session == nil {
		return nil
	}
	return session.dispatcher.Capabilities()
}

type sessionStatus uint8

const (
	sessionIdle sessionStatus = iota
	sessionActive
	sessionTerminated
)

type sessionState struct {
	mu     sync.Mutex
	status sessionStatus
}

func (s *sessionState) start() error {
	s.mu.Lock()
	defer s.mu.Unlock()

	switch s.status {
	case sessionIdle:
		s.status = sessionActive
		return nil
	case sessionActive:
		return ErrSessionActive
	case sessionTerminated:
		return ErrSessionTerminated
	default:
		panic("invalid session status")
	}
}

func (s *sessionState) finish(stopped bool) {
	s.mu.Lock()
	defer s.mu.Unlock()

	if s.status != sessionActive {
		panic("cannot finish inactive session")
	}
	if stopped {
		s.status = sessionTerminated
		return
	}
	s.status = sessionIdle
}

func (s *sessionState) terminate() error {
	s.mu.Lock()
	defer s.mu.Unlock()

	switch s.status {
	case sessionIdle:
		s.status = sessionTerminated
		return nil
	case sessionActive:
		return ErrSessionActive
	case sessionTerminated:
		return nil
	default:
		panic("invalid session status")
	}
}

// Close releases the Extism plugin instance owned by this session.
func (session *Session[K]) Close(ctx context.Context) error {
	if session == nil {
		return ErrSessionRequired
	}
	if err := session.state.terminate(); err != nil {
		return err
	}
	return session.plugin.Close(ctx)
}

// SessionStore owns per-key sessions.
type SessionStore[ID comparable, K SessionKey[ID]] interface {
	LoadSession(ctx context.Context, sessionID ID) (*Session[K], error)
	SaveSession(ctx context.Context, sessionID ID, session *Session[K]) error
}

// NewComputeCompiledPlugin compiles a module and registers the dispatcher host function.
func NewComputeCompiledPlugin[ID comparable, K SessionKey[ID]](ctx context.Context, config Config[ID, K]) (*ComputeCompiledPlugin[ID, K], error) {
	if config.SessionStore == nil {
		return nil, ErrSessionStoreRequired
	}

	compute := &ComputeCompiledPlugin[ID, K]{
		sessionStore:   config.SessionStore,
		instanceConfig: config.InstanceConfig,
	}

	runtimeConfig := config.PluginConfig.RuntimeConfig
	if runtimeConfig == nil {
		runtimeConfig = wazero.NewRuntimeConfig()
	}
	config.PluginConfig.RuntimeConfig = runtimeConfig.WithCloseOnContextDone(true)

	compiled, err := extism.NewCompiledPlugin(ctx, config.Manifest, config.PluginConfig, []extism.HostFunction{
		hostFunction(compute.sessionStore),
	})
	if err != nil {
		return nil, err
	}
	compute.compiled = compiled
	return compute, nil
}

// CloseCompiled releases the compiled Extism plugin without touching sessions or stores.
func (c *ComputeCompiledPlugin[ID, K]) CloseCompiled(ctx context.Context) error {
	return c.compiled.Close(ctx)
}

// PlayRequest is one guest invocation attempt.
type PlayRequest[ID comparable, K SessionKey[ID]] struct {
	Input      json.RawMessage
	Entrypoint string
	UserData   K
	Dispatcher dispatcher.Dispatcher[K]
}

// PlayStatus is the result of one guest invocation attempt.
type PlayStatus string

const (
	PlayCompleted PlayStatus = "completed"
	PlayYielded   PlayStatus = "yielded"
	PlayStopped   PlayStatus = "stopped"
	PlayFailed    PlayStatus = "failed"
)

// PlayResult is delivered when the play goroutine exits.
type PlayResult[K any] struct {
	Status PlayStatus
	Output json.RawMessage
	Exit   uint32
	Err    error
}

// PlayHandle controls one active guest invocation.
type PlayHandle[K any] struct {
	results chan PlayResult[K]
	cancel  context.CancelFunc

	mu            sync.Mutex
	finished      bool
	stopRequested bool
}

// Results returns the channel that receives exactly one result before closing.
func (h *PlayHandle[K]) Results() <-chan PlayResult[K] {
	if h == nil {
		return nil
	}
	return h.results
}

// Stop interrupts this invocation. It is safe to call Stop more than once.
func (h *PlayHandle[K]) Stop() {
	if h == nil {
		return
	}
	h.mu.Lock()
	if h.finished {
		h.mu.Unlock()
		return
	}
	h.stopRequested = true
	cancel := h.cancel
	h.mu.Unlock()
	cancel()
}

func (c *ComputeCompiledPlugin[ID, K]) CreateSession(ctx context.Context, request PlayRequest[ID, K]) (*Session[K], error) {
	plugin, err := c.compiled.Instance(ctx, c.instanceConfig)
	if err != nil {
		return nil, err
	}
	return &Session[K]{
		GuestData:  request.UserData,
		Input:      request.Input,
		Entrypoint: request.Entrypoint,
		plugin:     plugin,
		dispatcher: request.Dispatcher,
	}, nil
}

// Play invokes a session in its own goroutine and returns its control handle.
func (c *ComputeCompiledPlugin[ID, K]) Play(ctx context.Context, session *Session[K]) (*PlayHandle[K], error) {
	if session == nil {
		return nil, ErrSessionRequired
	}
	if err := session.state.start(); err != nil {
		return nil, err
	}

	sessionKey := session.GuestData.SessionKey()
	callCtx, cancel := context.WithCancel(ctx)
	handle := &PlayHandle[K]{
		results: make(chan PlayResult[K], 1),
		cancel:  cancel,
	}
	go func() {
		defer cancel()
		defer close(handle.results)

		pluginCtx := context.WithValue(callCtx, sessionKeyContextKey{}, sessionKey)
		exit, output, err := session.plugin.CallWithContext(pluginCtx, session.Entrypoint, session.Input)

		handle.mu.Lock()
		stopped := handle.stopRequested || ctx.Err() != nil
		handle.finished = true
		handle.mu.Unlock()

		session.state.finish(stopped)

		if stopped {
			stopErr := callCtx.Err()
			if stopErr == nil {
				stopErr = context.Canceled
			}
			handle.results <- PlayResult[K]{Status: PlayStopped, Exit: exit, Err: stopErr}
			return
		}
		if err != nil {
			handle.results <- PlayResult[K]{Status: PlayFailed, Exit: exit, Err: err}
			return
		}
		status, statusErr := playStatus(output)
		if statusErr != nil {
			handle.results <- PlayResult[K]{
				Status: PlayFailed,
				Output: append(json.RawMessage(nil), output...),
				Exit:   exit,
				Err:    statusErr,
			}
			return
		}
		if status == PlayYielded {
			handle.results <- PlayResult[K]{
				Status: PlayYielded,
				Output: append(json.RawMessage(nil), output...),
				Exit:   exit,
			}
			return
		}
		handle.results <- PlayResult[K]{
			Status: PlayCompleted,
			Output: append(json.RawMessage(nil), output...),
			Exit:   exit,
		}
	}()
	return handle, nil
}

func playStatus(output []byte) (PlayStatus, error) {
	var envelope struct {
		Status PlayStatus `json:"status"`
	}
	if err := json.Unmarshal(output, &envelope); err != nil {
		return PlayFailed, errors.Join(ErrInvalidGuestOutput, err)
	}
	switch envelope.Status {
	case PlayCompleted:
		return PlayCompleted, nil
	case PlayYielded:
		return PlayYielded, nil
	default:
		return PlayFailed, errors.Join(
			ErrInvalidGuestOutput,
			errors.New("status must be completed or yielded"),
		)
	}
}
