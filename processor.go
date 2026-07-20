package capcompute

import (
	"context"
	"encoding/json"
	"errors"
	"sync"
	"time"

	extism "github.com/extism/go-sdk"
	"github.com/tetratelabs/wazero"

	"github.com/aurora-capcompute/capcompute/sys"
)

var (
	ErrAmbientAuthority    = errors.New("image grants ambient authority")
	ErrImageMemoryOverride = errors.New("image overrides the program-owned memory cap")
	ErrInvalidGuestOutput  = errors.New("invalid guest output")
	ErrProcessActive       = errors.New("process is already active")
	ErrProcessRequired     = errors.New("process is required")
	ErrProcessTerminated   = errors.New("process is terminated")
)

// PID is the identity contract layers above key on (taints, schedulers,
// process maps): a credential that can name its process. The processor itself
// runs anonymous processes and never requires it. (Compare Go's stdlib
// `error` interface: the type and its one method share a name.)
type PID[ID comparable] interface {
	PID() ID
}

// Config contains everything needed to compile a program image.
//
// Image is the program image (an Extism manifest naming the wasm and static
// config). It must not grant ambient authority: AllowedHosts and AllowedPaths
// must be empty — network and filesystem access are capabilities served by the
// dispatcher, never ambient rights. Guest module configuration (clock, RNG,
// env) is owned by the processor and cannot be supplied by the caller.
type Config struct {
	Image        extism.Manifest
	PluginConfig extism.PluginConfig

	// MaxMemoryPages caps each process's linear memory in 64KiB wasm pages
	// (0 = runtime default). A guest allocating past the cap traps and the
	// resume reports ResumeFailed.
	MaxMemoryPages uint32
}

// Program is one compiled program image: a factory of processes. Many
// processes may run one program, and they need not share a credential type —
// the image is just code, so the credential lives on the Process.
type Program struct {
	compiled *extism.CompiledPlugin
}

// Process owns the reusable Extism plugin instance for one spawned program.
// Process state is not thread-safe; callers coordinate concurrent use.
//
// Cred is the host-side credential for the process: it identifies the process
// and carries whatever authority context the app attaches. It is never
// visible to or supplied by the guest.
type Process[K any] struct {
	Cred       K
	Input      json.RawMessage
	Entrypoint string
	plugin     *extism.Plugin
	dispatcher sys.Dispatcher[K]
	ambient    *ambientSources
	timeout    time.Duration
	state      processState
}

type processStatus uint8

const (
	processIdle processStatus = iota
	processActive
	processTerminated
)

type processState struct {
	mu     sync.Mutex
	status processStatus
}

func (s *processState) start() error {
	s.mu.Lock()
	defer s.mu.Unlock()

	switch s.status {
	case processIdle:
		s.status = processActive
		return nil
	case processActive:
		return ErrProcessActive
	case processTerminated:
		return ErrProcessTerminated
	default:
		panic("invalid process status")
	}
}

func (s *processState) finish(stopped bool) {
	s.mu.Lock()
	defer s.mu.Unlock()

	if s.status != processActive {
		panic("cannot finish inactive process")
	}
	if stopped {
		s.status = processTerminated
		return
	}
	s.status = processIdle
}

func (s *processState) terminate() error {
	s.mu.Lock()
	defer s.mu.Unlock()

	switch s.status {
	case processIdle:
		s.status = processTerminated
		return nil
	case processActive:
		return ErrProcessActive
	case processTerminated:
		return nil
	default:
		panic("invalid process status")
	}
}

// Close releases the Extism plugin instance owned by this process.
func (process *Process[K]) Close(ctx context.Context) error {
	if process == nil {
		return ErrProcessRequired
	}
	if err := process.state.terminate(); err != nil {
		return err
	}
	return process.plugin.Close(ctx)
}

// NewProgram compiles a program image and registers the syscall host
// function. It rejects images that grant ambient authority (non-empty
// AllowedHosts or AllowedPaths) with ErrAmbientAuthority.
func NewProgram(ctx context.Context, config Config) (*Program, error) {
	if len(config.Image.AllowedHosts) > 0 {
		return nil, errors.Join(ErrAmbientAuthority, errors.New("image sets allowed_hosts; network access must be a dispatched capability"))
	}
	if len(config.Image.AllowedPaths) > 0 {
		return nil, errors.Join(ErrAmbientAuthority, errors.New("image sets allowed_paths; filesystem access must be a dispatched capability"))
	}
	// The linear-memory cap is processor-owned (Config.MaxMemoryPages). An
	// image that also sets memory.max_pages would silently win (the SDK applies
	// it last), so an image trying to set its own ceiling — raising it above
	// the configured cap, or claiming ownership of a limit the processor
	// guarantees — is refused. Other manifest memory knobs (var/http byte caps)
	// are untouched.
	if config.Image.Memory != nil && config.Image.Memory.MaxPages > 0 {
		return nil, errors.Join(ErrImageMemoryOverride, errors.New("image sets memory.max_pages; the memory cap is program-owned via Config.MaxMemoryPages"))
	}

	runtimeConfig := config.PluginConfig.RuntimeConfig
	if runtimeConfig == nil {
		runtimeConfig = wazero.NewRuntimeConfig()
	}
	runtimeConfig = runtimeConfig.WithCloseOnContextDone(true)
	if config.MaxMemoryPages > 0 {
		runtimeConfig = runtimeConfig.WithMemoryLimitPages(config.MaxMemoryPages)
	}
	config.PluginConfig.RuntimeConfig = runtimeConfig

	compiled, err := extism.NewCompiledPlugin(ctx, config.Image, config.PluginConfig, []extism.HostFunction{
		hostFunction(),
	})
	if err != nil {
		return nil, err
	}
	return &Program{compiled: compiled}, nil
}

// Close releases the compiled program image without touching processes.
func (p *Program) Close(ctx context.Context) error {
	return p.compiled.Close(ctx)
}

// ProcessSpec describes one process: its program input, entrypoint,
// credential, the dispatcher that will serve its syscalls, and its quantum
// deadline.
type ProcessSpec[K any] struct {
	Input      json.RawMessage
	Entrypoint string
	Cred       K
	Dispatcher sys.Dispatcher[K]

	// ResumeTimeout bounds the wall-clock time of one Resume quantum
	// (0 = unbounded). A guest still running at the deadline is stopped and
	// the resume reports ResumeStopped with context.DeadlineExceeded.
	ResumeTimeout time.Duration
}

// ResumeStatus is the result of one guest invocation attempt.
type ResumeStatus string

const (
	ResumeCompleted ResumeStatus = "completed"
	ResumeYielded   ResumeStatus = "yielded"
	ResumeStopped   ResumeStatus = "stopped"
	ResumeFailed    ResumeStatus = "failed"
)

// ResumeResult is delivered when the resume goroutine exits.
type ResumeResult struct {
	Status ResumeStatus
	Output json.RawMessage
	Exit   uint32
	Err    error
}

// ResumeHandle controls one active guest invocation.
type ResumeHandle struct {
	results chan ResumeResult
	cancel  context.CancelFunc

	mu            sync.Mutex
	finished      bool
	stopRequested bool
}

// Results returns the channel that receives exactly one result before closing.
func (h *ResumeHandle) Results() <-chan ResumeResult {
	if h == nil {
		return nil
	}
	return h.results
}

// Stop interrupts this invocation. It is safe to call Stop more than once.
func (h *ResumeHandle) Stop() {
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

// NewProcess instantiates a fresh process from the program image —
// posix_spawn semantics: a named program, explicit input, explicitly handed
// authority (the dispatcher), nothing inherited. The guest-side counterpart
// is the reserved sys.spawn syscall, served above the processor by whatever
// spawner the runtime composes.
func NewProcess[K any](ctx context.Context, program *Program, spec ProcessSpec[K]) (*Process[K], error) {
	moduleConfig, ambient := guestModuleConfig()
	plugin, err := program.compiled.Instance(ctx, extism.PluginInstanceConfig{
		ModuleConfig: moduleConfig,
	})
	if err != nil {
		return nil, err
	}
	return &Process[K]{
		Cred:       spec.Cred,
		Input:      spec.Input,
		Entrypoint: spec.Entrypoint,
		plugin:     plugin,
		dispatcher: spec.Dispatcher,
		ambient:    ambient,
		timeout:    spec.ResumeTimeout,
	}, nil
}

// Resume gives a process the CPU in its own goroutine and returns its control
// handle. The process runs until it completes, yields, fails, or is stopped —
// cooperative scheduling, no preemption.
func Resume[K any](ctx context.Context, process *Process[K]) (*ResumeHandle, error) {
	if process == nil {
		return nil, ErrProcessRequired
	}
	if err := process.state.start(); err != nil {
		return nil, err
	}
	// A resume re-executes the entrypoint from the top on a possibly-warm
	// instance; reset the pinned clock/RNG so the re-execution observes the same
	// ambient sequence a cold replay would (kernel law #2). No quantum for this
	// process runs concurrently — state.start above serializes them — so this is
	// safe to do before launching the run.
	process.ambient.reset()

	var (
		callCtx context.Context
		cancel  context.CancelFunc
	)
	if process.timeout > 0 {
		callCtx, cancel = context.WithTimeout(ctx, process.timeout)
	} else {
		callCtx, cancel = context.WithCancel(ctx)
	}
	handle := &ResumeHandle{
		results: make(chan ResumeResult, 1),
		cancel:  cancel,
	}
	go func() {
		defer cancel()
		defer close(handle.results)

		// Bind this process's credential and dispatcher into one closure: the
		// host function needs nothing else, so the syscall path carries no
		// process and stays free of the credential type.
		dispatch := syscallFunc(func(ctx context.Context, syscall sys.Syscall) (sys.SyscallResult, error) {
			return process.dispatcher.Dispatch(ctx, process.Cred, syscall, sys.Authorization{})
		})
		pluginCtx := context.WithValue(callCtx, syscallContextKey{}, dispatch)
		exit, output, err := process.plugin.CallWithContext(pluginCtx, process.Entrypoint, process.Input)

		handle.mu.Lock()
		stopped := handle.stopRequested || callCtx.Err() != nil
		handle.finished = true
		handle.mu.Unlock()

		process.state.finish(stopped)

		if stopped {
			stopErr := callCtx.Err()
			if stopErr == nil {
				stopErr = context.Canceled
			}
			handle.results <- ResumeResult{Status: ResumeStopped, Exit: exit, Err: stopErr}
			return
		}
		if err != nil {
			handle.results <- ResumeResult{Status: ResumeFailed, Exit: exit, Err: err}
			return
		}
		status, statusErr := resumeStatus(output)
		if statusErr != nil {
			handle.results <- ResumeResult{
				Status: ResumeFailed,
				Output: append(json.RawMessage(nil), output...),
				Exit:   exit,
				Err:    statusErr,
			}
			return
		}
		if status == ResumeYielded {
			handle.results <- ResumeResult{
				Status: ResumeYielded,
				Output: append(json.RawMessage(nil), output...),
				Exit:   exit,
			}
			return
		}
		handle.results <- ResumeResult{
			Status: ResumeCompleted,
			Output: append(json.RawMessage(nil), output...),
			Exit:   exit,
		}
	}()
	return handle, nil
}

func resumeStatus(output []byte) (ResumeStatus, error) {
	var envelope struct {
		Status ResumeStatus `json:"status"`
	}
	if err := json.Unmarshal(output, &envelope); err != nil {
		return ResumeFailed, errors.Join(ErrInvalidGuestOutput, err)
	}
	switch envelope.Status {
	case ResumeCompleted:
		return ResumeCompleted, nil
	case ResumeYielded:
		return ResumeYielded, nil
	default:
		return ResumeFailed, errors.Join(
			ErrInvalidGuestOutput,
			errors.New("status must be completed or yielded"),
		)
	}
}
