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
	ErrAmbientAuthority     = errors.New("image grants ambient authority")
	ErrInvalidGuestOutput   = errors.New("invalid guest output")
	ErrProcessActive        = errors.New("process is already active")
	ErrProcessRequired      = errors.New("process is required")
	ErrProcessTableRequired = errors.New("process table is required")
	ErrProcessTerminated    = errors.New("process is terminated")
)

// PID lets user-owned process data expose the stable process identity used for
// process maps. (Compare Go's stdlib `error` interface: the type and its one
// method share a name.)
type PID[ID comparable] interface {
	PID() ID
}

// Config contains everything needed to compile a program and create per-process instances.
//
// Image is the program image (an Extism manifest naming the wasm and static
// config). It must not grant ambient authority: AllowedHosts and AllowedPaths
// must be empty — network and filesystem access are capabilities served by the
// dispatcher, never ambient rights. Guest module configuration (clock, RNG,
// env) is owned by the kernel and cannot be supplied by the caller.
type Config[ID comparable, K PID[ID]] struct {
	Image        extism.Manifest
	PluginConfig extism.PluginConfig
	ProcessTable ProcessTable[ID, K]

	// MaxMemoryPages caps each process's linear memory in 64KiB wasm pages
	// (0 = runtime default). A guest allocating past the cap traps and the
	// resume reports ResumeFailed.
	MaxMemoryPages uint32

	// ResumeTimeout bounds the wall-clock time of one Resume quantum
	// (0 = unbounded). A guest still running at the deadline is stopped and
	// the resume reports ResumeStopped with context.DeadlineExceeded.
	ResumeTimeout time.Duration
}

// Kernel owns one compiled program image and spawns processes from it.
type Kernel[ID comparable, K PID[ID]] struct {
	compiled      *extism.CompiledPlugin
	processTable  ProcessTable[ID, K]
	resumeTimeout time.Duration
}

// Process owns the reusable Extism plugin instance for one PID.
// Process state is not thread-safe; callers coordinate concurrent use.
//
// Cred is the host-side credential for the process: it identifies the process
// (PID) and carries whatever authority context the app attaches. It is never
// visible to or supplied by the guest.
type Process[K any] struct {
	Cred       K
	Input      json.RawMessage
	Entrypoint string
	plugin     *extism.Plugin
	dispatcher sys.Dispatcher[K]
	state      processState
}

// Capabilities returns the operations exposed by this process's dispatcher.
func (process *Process[K]) Capabilities() []sys.Capability {
	if process == nil {
		return nil
	}
	return process.dispatcher.Capabilities()
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

// ProcessTable owns per-PID processes.
type ProcessTable[ID comparable, K PID[ID]] interface {
	LoadProcess(ctx context.Context, pid ID) (*Process[K], error)
	SaveProcess(ctx context.Context, pid ID, process *Process[K]) error
}

// NewKernel compiles a program and registers the syscall host function. It
// rejects images that grant ambient authority (non-empty AllowedHosts or
// AllowedPaths) with ErrAmbientAuthority.
func NewKernel[ID comparable, K PID[ID]](ctx context.Context, config Config[ID, K]) (*Kernel[ID, K], error) {
	if config.ProcessTable == nil {
		return nil, ErrProcessTableRequired
	}
	if len(config.Image.AllowedHosts) > 0 {
		return nil, errors.Join(ErrAmbientAuthority, errors.New("image sets allowed_hosts; network access must be a dispatched capability"))
	}
	if len(config.Image.AllowedPaths) > 0 {
		return nil, errors.Join(ErrAmbientAuthority, errors.New("image sets allowed_paths; filesystem access must be a dispatched capability"))
	}

	kernel := &Kernel[ID, K]{
		processTable:  config.ProcessTable,
		resumeTimeout: config.ResumeTimeout,
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
		hostFunction(kernel.processTable),
	})
	if err != nil {
		return nil, err
	}
	kernel.compiled = compiled
	return kernel, nil
}

// Shutdown releases the compiled program image without touching processes or tables.
func (k *Kernel[ID, K]) Shutdown(ctx context.Context) error {
	return k.compiled.Close(ctx)
}

// ProcessSpec describes one process to create: its program input, entrypoint,
// credential, and the dispatcher that will serve its syscalls.
type ProcessSpec[ID comparable, K PID[ID]] struct {
	Input      json.RawMessage
	Entrypoint string
	Cred       K
	Dispatcher sys.Dispatcher[K]
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
type ResumeResult[K any] struct {
	Status ResumeStatus
	Output json.RawMessage
	Exit   uint32
	Err    error
}

// ResumeHandle controls one active guest invocation.
type ResumeHandle[K any] struct {
	results chan ResumeResult[K]
	cancel  context.CancelFunc

	mu            sync.Mutex
	finished      bool
	stopRequested bool
}

// Results returns the channel that receives exactly one result before closing.
func (h *ResumeHandle[K]) Results() <-chan ResumeResult[K] {
	if h == nil {
		return nil
	}
	return h.results
}

// Stop interrupts this invocation. It is safe to call Stop more than once.
func (h *ResumeHandle[K]) Stop() {
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

// CreateProcess instantiates a fresh process from the kernel's program image.
// It does not save the process; the caller decides when it becomes visible in
// the process table.
func (k *Kernel[ID, K]) CreateProcess(ctx context.Context, spec ProcessSpec[ID, K]) (*Process[K], error) {
	plugin, err := k.compiled.Instance(ctx, extism.PluginInstanceConfig{
		ModuleConfig: guestModuleConfig(),
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
	}, nil
}

// Resume gives a process the CPU in its own goroutine and returns its control
// handle. The process runs until it completes, yields, fails, or is stopped —
// cooperative scheduling, no preemption.
func (k *Kernel[ID, K]) Resume(ctx context.Context, process *Process[K]) (*ResumeHandle[K], error) {
	if process == nil {
		return nil, ErrProcessRequired
	}
	if err := process.state.start(); err != nil {
		return nil, err
	}

	pid := process.Cred.PID()
	callCtx, cancel := context.WithCancel(ctx)
	if k.resumeTimeout > 0 {
		callCtx, cancel = context.WithTimeout(ctx, k.resumeTimeout)
	}
	handle := &ResumeHandle[K]{
		results: make(chan ResumeResult[K], 1),
		cancel:  cancel,
	}
	go func() {
		defer cancel()
		defer close(handle.results)

		pluginCtx := context.WithValue(callCtx, pidContextKey{}, pid)
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
			handle.results <- ResumeResult[K]{Status: ResumeStopped, Exit: exit, Err: stopErr}
			return
		}
		if err != nil {
			handle.results <- ResumeResult[K]{Status: ResumeFailed, Exit: exit, Err: err}
			return
		}
		status, statusErr := resumeStatus(output)
		if statusErr != nil {
			handle.results <- ResumeResult[K]{
				Status: ResumeFailed,
				Output: append(json.RawMessage(nil), output...),
				Exit:   exit,
				Err:    statusErr,
			}
			return
		}
		if status == ResumeYielded {
			handle.results <- ResumeResult[K]{
				Status: ResumeYielded,
				Output: append(json.RawMessage(nil), output...),
				Exit:   exit,
			}
			return
		}
		handle.results <- ResumeResult[K]{
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
