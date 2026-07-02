package capcompute

import (
	"context"
	"encoding/json"
	"fmt"

	"github.com/aurora-capcompute/capcompute/sys"
)

// SpawnRequest is the args payload of the reserved sys.spawn syscall.
type SpawnRequest struct {
	Program    string          `json:"program"`
	Entrypoint string          `json:"entrypoint,omitempty"` // defaults to "run"
	Input      json.RawMessage `json:"input,omitempty"`
	// Capabilities names the grants the child receives. Attenuation law:
	// every name must be in the parent's own grant set.
	Capabilities []string `json:"capabilities,omitempty"`
}

var spawnInputSchema = json.RawMessage(`{
	"type": "object",
	"required": ["program"],
	"properties": {
		"program":      {"type": "string", "minLength": 1},
		"entrypoint":   {"type": "string"},
		"input":        {},
		"capabilities": {"type": "array", "items": {"type": "string"}}
	}
}`)

// ChildRunner runs one child process until it completes, yields, fails, or is
// stopped. The child's own journal (keyed by the child cred's PID) and its
// dispatcher chain are the runner's to wire; KernelChildRunner is the
// kernel-backed implementation.
type ChildRunner[K any] func(ctx context.Context, child K, granted []sys.Capability, request SpawnRequest) (ResumeResult[K], error)

// SpawnConfig wires the Spawner decorator.
type SpawnConfig[K any] struct {
	// Grants reports the parent's capability set — the ceiling a child's
	// grants are attenuated under.
	Grants GrantSource[K]
	// DeriveCred mints the child credential. spawnKey is the spawn syscall's
	// idempotency key — the intent identity (run, position, call-hash) — so
	// the derived child is deterministic: a replayed or re-entered spawn
	// derives the same child, which is what lets a yielded child's journal be
	// found again on resume.
	DeriveCred func(parent K, spawnKey string, program string) K
	// Run executes the child.
	Run ChildRunner[K]
}

// Spawner is the kernel-provided dispatcher decorator serving sys.spawn:
// capability attenuation, deterministic child identity, and the sync-first
// child-workflow protocol (the child borrows the parent's quantum; a yielding
// child yields the parent transitively). It must sit *below* the parent's
// replay layer: a completed spawn is then journaled like any syscall, so
// replay serves the child's result without re-spawning, and the spawn's
// idempotency key is available for cred derivation.
type Spawner[K any] struct {
	config SpawnConfig[K]
	next   sys.Dispatcher[K]
}

func NewSpawner[K any](config SpawnConfig[K], next sys.Dispatcher[K]) *Spawner[K] {
	return &Spawner[K]{config: config, next: next}
}

func (s *Spawner[K]) Dispatch(ctx context.Context, cred K, syscall sys.Syscall, auth sys.Authorization) (sys.SyscallResult, error) {
	if syscall.Name != sys.SyscallSpawn {
		return s.next.Dispatch(ctx, cred, syscall, auth)
	}

	var request SpawnRequest
	if err := json.Unmarshal(syscall.Args, &request); err != nil {
		return sys.FailCode(sys.ErrnoInvalidArgs, fmt.Sprintf("decode spawn args: %v", err)), nil
	}
	if request.Program == "" {
		return sys.FailCode(sys.ErrnoInvalidArgs, "spawn: program is required"), nil
	}
	if request.Entrypoint == "" {
		request.Entrypoint = "run"
	}

	// Attenuation: resolve requested names against the parent's grant set —
	// a parent cannot give what it does not hold.
	parentGrants := s.config.Grants(cred)
	requested := make([]sys.Capability, 0, len(request.Capabilities))
	for _, name := range request.Capabilities {
		granted, ok := findCapability(parentGrants, name)
		if !ok {
			return sys.FailCode(sys.ErrnoDenied, fmt.Sprintf("spawn: parent does not hold capability %q", name)), nil
		}
		requested = append(requested, granted)
	}
	granted, err := sys.Attenuate(parentGrants, requested)
	if err != nil {
		return sys.FailCode(sys.ErrnoDenied, fmt.Sprintf("spawn: %v", err)), nil
	}

	spawnKey, ok := sys.IdempotencyKey(ctx)
	if !ok {
		// Without the intent identity the child would not be deterministic.
		return sys.SyscallResult{}, fmt.Errorf("spawn: no idempotency key in context; Spawner must run below the replay layer")
	}
	child := s.config.DeriveCred(cred, spawnKey, request.Program)

	result, err := s.config.Run(ctx, child, granted, request)
	if err != nil {
		return sys.SyscallResult{}, fmt.Errorf("spawn: run child: %w", err)
	}
	switch result.Status {
	case ResumeCompleted:
		return sys.Result(result.Output), nil
	case ResumeYielded:
		// Transitive yield: the parent blocks on the same external work the
		// child blocks on. On resume the parent replays, re-issues this
		// spawn, and the deterministically derived child re-enters its own
		// journal.
		return sys.Yield("child process yielded"), nil
	case ResumeFailed:
		message := "child process failed"
		if result.Err != nil {
			message = fmt.Sprintf("child process failed: %v", result.Err)
		}
		return sys.FailCode(sys.ErrnoInternal, message), nil
	default: // ResumeStopped: host interruption, outcome unknown — keep the intent open.
		err := result.Err
		if err == nil {
			err = context.Canceled
		}
		return sys.SyscallResult{}, fmt.Errorf("spawn: child stopped: %w", err)
	}
}

// Capabilities exposes sys.spawn (with its input schema) alongside the
// chain's own capabilities, so grant-set validation and discovery see it.
func (s *Spawner[K]) Capabilities() []sys.Capability {
	return append(s.next.Capabilities(), sys.Capability{
		Name:        sys.SyscallSpawn,
		Description: "create a child process with an attenuated capability set; blocks until the child completes or yields",
		InputSchema: spawnInputSchema,
	})
}

// KernelChildRunner runs children on a kernel: it creates the child process,
// registers it in the process table (the syscall path finds it by PID), gives
// it the CPU, and closes it once its quantum ends. childDispatcher wires the
// child's own chain — its journal tape (keyed by the child's PID), validator,
// and drivers over exactly the granted set.
func KernelChildRunner[ID comparable, K PID[ID]](
	kernel *Kernel[ID, K],
	childDispatcher func(ctx context.Context, child K, granted []sys.Capability) (sys.Dispatcher[K], error),
) ChildRunner[K] {
	return func(ctx context.Context, child K, granted []sys.Capability, request SpawnRequest) (ResumeResult[K], error) {
		dispatcher, err := childDispatcher(ctx, child, granted)
		if err != nil {
			return ResumeResult[K]{}, err
		}
		process, err := kernel.CreateProcess(ctx, ProcessSpec[ID, K]{
			Input:      request.Input,
			Entrypoint: request.Entrypoint,
			Cred:       child,
			Dispatcher: dispatcher,
		})
		if err != nil {
			return ResumeResult[K]{}, err
		}
		if err := kernel.processTable.SaveProcess(ctx, child.PID(), process); err != nil {
			_ = process.Close(ctx)
			return ResumeResult[K]{}, err
		}
		handle, err := kernel.Resume(ctx, process)
		if err != nil {
			_ = process.Close(ctx)
			return ResumeResult[K]{}, err
		}
		result := <-handle.Results()
		_ = process.Close(ctx) // the instance is per-quantum; the journal is the durable child
		return result, nil
	}
}
