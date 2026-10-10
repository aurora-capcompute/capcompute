package capcompute

import (
	"context"
	"encoding/json"
	"errors"
	"sync"

	extism "github.com/extism/go-sdk"
	"github.com/tetratelabs/wazero"

	"github.com/aurora-capcompute/capcompute/sys"
)

var (
	ErrAmbientAuthority    = errors.New("image grants ambient authority")
	ErrImageMemoryOverride = errors.New("image overrides the program-owned memory cap")
	ErrInvalidGuestOutput  = errors.New("invalid guest output")
	ErrProcessActive       = errors.New("process is already active")
	ErrProcessTerminated   = errors.New("process is terminated")
)

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
// credential, and the dispatcher that will serve its syscalls.
//
// Cred is the host-side credential for the process: it identifies the process
// and carries whatever authority context the app attaches. It is never
// visible to or supplied by the guest.
type ProcessSpec[K any] struct {
	Input      json.RawMessage
	Entrypoint string
	Cred       K
	Dispatcher sys.Dispatcher[K]
}

// Process owns the reusable Extism plugin instance for one spawned program.
// Its quanta are serialized: a second Resume while one runs is refused.
type Process[K any] struct {
	spec    ProcessSpec[K]
	plugin  *extism.Plugin
	ambient *ambientSources
	state   processState
}

// NewProcess instantiates a fresh process from the program image —
// posix_spawn semantics: a named program, explicit input, explicitly handed
// authority (the dispatcher), nothing inherited.
func NewProcess[K any](ctx context.Context, program *Program, spec ProcessSpec[K]) (*Process[K], error) {
	moduleConfig, ambient := guestModuleConfig()
	plugin, err := program.compiled.Instance(ctx, extism.PluginInstanceConfig{
		ModuleConfig: moduleConfig,
	})
	if err != nil {
		return nil, err
	}
	return &Process[K]{spec: spec, plugin: plugin, ambient: ambient}, nil
}

// Close releases the Extism plugin instance owned by this process.
func (process *Process[K]) Close(ctx context.Context) error {
	if err := process.state.terminate(); err != nil {
		return err
	}
	return process.plugin.Close(ctx)
}

// ResumeStatus is how one quantum ended.
type ResumeStatus string

const (
	ResumeCompleted ResumeStatus = "completed"
	ResumeYielded   ResumeStatus = "yielded"
	ResumeStopped   ResumeStatus = "stopped"
	ResumeFailed    ResumeStatus = "failed"
)

// ResumeResult is how one quantum ended. Output is the guest's own output. Err
// says why a quantum failed or was stopped.
type ResumeResult struct {
	Status ResumeStatus
	Output json.RawMessage
	Err    error
}

// Resume gives a process the CPU in its own goroutine and returns the channel
// its outcome arrives on. The process runs until it completes, yields, fails,
// or is stopped — cooperative scheduling, no preemption. ctx is the only way to
// stop it: a guest still running when ctx ends is stopped, and a stopped
// process is terminated.
func Resume[K any](ctx context.Context, process *Process[K]) (<-chan ResumeResult, error) {
	if err := process.state.start(); err != nil {
		return nil, err
	}
	// A resume runs the entrypoint again from the top, often on a warm instance
	// whose pinned clock and RNG already advanced. Reset them, so the run sees
	// the same ambient sequence a cold replay would (law #2). state.start
	// guarantees no other quantum of this process is running.
	process.ambient.reset()

	results := make(chan ResumeResult, 1)
	go func() {
		defer close(results)
		results <- process.run(ctx)
	}()
	return results, nil
}

// run calls the entrypoint once and reports how the call ended.
func (process *Process[K]) run(ctx context.Context) ResumeResult {
	spec := process.spec
	// The host function finds this process's dispatch in the call context: the
	// dispatcher, already bound to the credential.
	dispatch := syscallFunc(func(ctx context.Context, syscall sys.Syscall) (sys.SyscallResult, error) {
		return spec.Dispatcher.Dispatch(ctx, spec.Cred, syscall, sys.Authorization{})
	})
	_, output, err := process.plugin.CallWithContext(
		context.WithValue(ctx, syscallContextKey{}, dispatch), spec.Entrypoint, spec.Input)

	stopped := ctx.Err() != nil
	process.state.finish(stopped)
	switch {
	case stopped:
		return ResumeResult{Status: ResumeStopped, Err: ctx.Err()}
	case err != nil:
		return ResumeResult{Status: ResumeFailed, Err: err}
	}
	status, err := resumeStatus(output)
	return ResumeResult{Status: status, Output: output, Err: err}
}

// resumeStatus reads the status a guest reports in its output: completed or
// yielded. Anything else is a failure.
func resumeStatus(output []byte) (ResumeStatus, error) {
	var envelope struct {
		Status ResumeStatus `json:"status"`
	}
	if err := json.Unmarshal(output, &envelope); err != nil {
		return ResumeFailed, errors.Join(ErrInvalidGuestOutput, err)
	}
	switch envelope.Status {
	case ResumeCompleted, ResumeYielded:
		return envelope.Status, nil
	}
	return ResumeFailed, errors.Join(ErrInvalidGuestOutput, errors.New("status must be completed or yielded"))
}

// processStatus is where a process is in its lifecycle.
type processStatus uint8

const (
	processIdle processStatus = iota
	processActive
	processTerminated
)

// processState is a process's lifecycle: idle between quanta, active during
// one, terminated once stopped or closed.
type processState struct {
	mu     sync.Mutex
	status processStatus
}

// start begins a quantum. It refuses a process that is running one already, or
// that is terminated.
func (s *processState) start() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	switch s.status {
	case processActive:
		return ErrProcessActive
	case processTerminated:
		return ErrProcessTerminated
	}
	s.status = processActive
	return nil
}

// finish ends the active quantum. A stopped quantum terminates the process.
func (s *processState) finish(stopped bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if stopped {
		s.status = processTerminated
	} else {
		s.status = processIdle
	}
}

// terminate ends the process for good. It refuses while a quantum runs.
func (s *processState) terminate() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.status == processActive {
		return ErrProcessActive
	}
	s.status = processTerminated
	return nil
}
