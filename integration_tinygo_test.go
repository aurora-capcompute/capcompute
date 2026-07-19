package capcompute_test

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	extism "github.com/extism/go-sdk"

	"github.com/aurora-capcompute/capcompute"
	"github.com/aurora-capcompute/capcompute/sys"
)

type integrationPID struct {
	id string
}

func (k integrationPID) PID() string {
	return k.id
}

type integrationDispatcher struct{}

func (integrationDispatcher) Dispatch(_ context.Context, _ integrationPID, syscall sys.Syscall, _ sys.Authorization) (sys.SyscallResult, error) {
	switch syscall.Name {
	case "host.echo":
		return sys.Result(json.RawMessage(`{"echoed":true}`)), nil
	case "host.yield":
		return sys.Yield("waiting for outside work"), nil
	default:
		return sys.SyscallResult{}, errors.New("unknown syscall")
	}
}

func (integrationDispatcher) Capabilities() []sys.Capability { return nil }

func TestTinyGoGuestResumeStates(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping TinyGo integration test in short mode")
	}
	if _, err := exec.LookPath("tinygo"); err != nil {
		t.Skip("tinygo not found")
	}

	ctx := context.Background()
	wasmPath := buildTinyGoIntegrationGuest(t)
	program, err := capcompute.NewProgram[integrationPID](ctx, capcompute.Config{
		Image: extism.Manifest{
			Wasm: []extism.Wasm{extism.WasmFile{Path: wasmPath}},
		},
		PluginConfig: extism.PluginConfig{
			EnableWasi: true,
		},
	})
	if err != nil {
		t.Fatalf("new program: %v", err)
	}
	t.Cleanup(func() {
		if err := program.Close(context.Background()); err != nil {
			t.Errorf("close program: %v", err)
		}
	})

	tests := []struct {
		name string
		want capcompute.ResumeStatus
	}{
		{name: "completed", want: capcompute.ResumeCompleted},
		{name: "yielded", want: capcompute.ResumeYielded},
		{name: "failed", want: capcompute.ResumeFailed},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			pid := integrationPID{id: "run-" + tt.name}
			input, err := json.Marshal(struct {
				Mode string `json:"mode"`
			}{
				Mode: tt.name,
			})
			if err != nil {
				t.Fatalf("encode input: %v", err)
			}
			process, err := capcompute.NewProcess(ctx, program, capcompute.ProcessSpec[integrationPID]{
				Input:      input,
				Entrypoint: "run",
				Cred:       pid,
				Dispatcher: integrationDispatcher{},
			})
			if err != nil {
				t.Fatalf("create process: %v", err)
			}
			t.Cleanup(func() {
				if err := process.Close(context.Background()); err != nil {
					t.Errorf("close process: %v", err)
				}
			})

			handle, err := capcompute.Resume(ctx, process)
			if err != nil {
				t.Fatalf("resume: %v", err)
			}
			result := <-handle.Results()
			if result.Status != tt.want {
				t.Fatalf("status = %s, want %s; err = %v; output = %s", result.Status, tt.want, result.Err, result.Output)
			}
			if tt.want == capcompute.ResumeFailed && result.Err == nil {
				t.Fatal("failed resume returned nil error")
			}
			if tt.want != capcompute.ResumeFailed && result.Err != nil {
				t.Fatalf("resume error: %v", result.Err)
			}
		})
	}
}

// An infrastructure error from the dispatcher is not an outcome: nothing was
// journaled, so the guest must never observe it — journal-before-observe
// covers the indeterminate case. The quantum traps and the resume fails with
// the dispatch error; the guest's infra mode would otherwise complete and
// report the error it saw.
func TestTinyGoGuestNeverObservesInfrastructureErrors(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping TinyGo integration test in short mode")
	}
	if _, err := exec.LookPath("tinygo"); err != nil {
		t.Skip("tinygo not found")
	}

	ctx := context.Background()
	wasmPath := buildTinyGoIntegrationGuest(t)
	program, err := capcompute.NewProgram[integrationPID](ctx, capcompute.Config{
		Image:        extism.Manifest{Wasm: []extism.Wasm{extism.WasmFile{Path: wasmPath}}},
		PluginConfig: extism.PluginConfig{EnableWasi: true},
	})
	if err != nil {
		t.Fatalf("new program: %v", err)
	}
	t.Cleanup(func() {
		if err := program.Close(context.Background()); err != nil {
			t.Errorf("close program: %v", err)
		}
	})

	pid := integrationPID{id: "run-infra"}
	process, err := capcompute.NewProcess(ctx, program, capcompute.ProcessSpec[integrationPID]{
		Input:      json.RawMessage(`{"mode":"infra"}`),
		Entrypoint: "run",
		Cred:       pid,
		Dispatcher: integrationDispatcher{},
	})
	if err != nil {
		t.Fatalf("create process: %v", err)
	}
	t.Cleanup(func() {
		if err := process.Close(context.Background()); err != nil {
			t.Errorf("close process: %v", err)
		}
	})

	handle, err := capcompute.Resume(ctx, process)
	if err != nil {
		t.Fatalf("resume: %v", err)
	}
	result := <-handle.Results()
	if result.Status != capcompute.ResumeFailed {
		t.Fatalf("status = %s (output %s), want failed — the guest observed an unjournaled outcome", result.Status, result.Output)
	}
	if result.Err == nil || !strings.Contains(result.Err.Error(), "unknown syscall") {
		t.Fatalf("err = %v, want the dispatch error surfaced", result.Err)
	}
}

func TestTinyGoGuestCanBeStopped(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping TinyGo integration test in short mode")
	}
	if _, err := exec.LookPath("tinygo"); err != nil {
		t.Skip("tinygo not found")
	}

	ctx := context.Background()
	wasmPath := buildTinyGoIntegrationGuest(t)
	program, err := capcompute.NewProgram[integrationPID](ctx, capcompute.Config{
		Image: extism.Manifest{
			Wasm: []extism.Wasm{extism.WasmFile{Path: wasmPath}},
		},
		PluginConfig: extism.PluginConfig{
			EnableWasi: true,
		},
	})
	if err != nil {
		t.Fatalf("new program: %v", err)
	}
	t.Cleanup(func() {
		if err := program.Close(context.Background()); err != nil {
			t.Errorf("close program: %v", err)
		}
	})

	pid := integrationPID{id: "run-stopped"}
	input, err := json.Marshal(struct {
		Mode string `json:"mode"`
	}{
		Mode: "infinite",
	})
	if err != nil {
		t.Fatalf("encode input: %v", err)
	}
	process, err := capcompute.NewProcess(ctx, program, capcompute.ProcessSpec[integrationPID]{
		Input:      input,
		Entrypoint: "run",
		Cred:       pid,
		Dispatcher: integrationDispatcher{},
	})
	if err != nil {
		t.Fatalf("create process: %v", err)
	}
	t.Cleanup(func() {
		if err := process.Close(context.Background()); err != nil {
			t.Errorf("close process: %v", err)
		}
	})

	handle, err := capcompute.Resume(ctx, process)
	if err != nil {
		t.Fatalf("resume: %v", err)
	}
	if _, err := capcompute.Resume(ctx, process); err != capcompute.ErrProcessActive {
		t.Fatalf("concurrent resume error = %v, want ErrProcessActive", err)
	}

	handle.Stop()
	handle.Stop()

	results := handle.Results()
	select {
	case result := <-results:
		if result.Status != capcompute.ResumeStopped {
			t.Fatalf("status = %s, want %s; err = %v", result.Status, capcompute.ResumeStopped, result.Err)
		}
		if !errors.Is(result.Err, context.Canceled) {
			t.Fatalf("error = %v, want context canceled", result.Err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("stopped resume did not return")
	}
	if _, ok := <-results; ok {
		t.Fatal("stopped resume returned more than one result")
	}

	if _, err := capcompute.Resume(ctx, process); err != capcompute.ErrProcessTerminated {
		t.Fatalf("resume error = %v, want ErrProcessTerminated", err)
	}
}

func buildTinyGoIntegrationGuest(t *testing.T) string {
	t.Helper()

	wasmPath := filepath.Join(t.TempDir(), "integration_guest.wasm")
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()

	cmd := exec.CommandContext(
		ctx,
		"tinygo",
		"build",
		"-target", "wasip1",
		"-buildmode=c-shared",
		"-tags", "tinygo",
		"-o", wasmPath,
		"./testdata/integration_guest",
	)
	cmd.Env = append(os.Environ(),
		"XDG_CACHE_HOME="+t.TempDir(),
		"GOCACHE="+filepath.Join(t.TempDir(), "go-build"),
	)
	out, err := cmd.CombinedOutput()
	if ctx.Err() != nil {
		t.Fatalf("build integration guest timed out: %v", ctx.Err())
	}
	if err != nil {
		t.Fatalf("build integration guest: %v\n%s", err, strings.TrimSpace(string(out)))
	}
	return wasmPath
}

// Kernel law #2 (determinism): two fresh processes running the ambient mode —
// which reads the WASI clock and RNG the kernel pins — must observe identical
// values. A crash-replay is exactly a fresh process re-running the same code,
// so equality here is what makes un-journaled ambient reads safe.
func TestTinyGoGuestAmbientReadsAreDeterministic(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping TinyGo integration test in short mode")
	}
	if _, err := exec.LookPath("tinygo"); err != nil {
		t.Skip("tinygo not found")
	}

	ctx := context.Background()
	wasmPath := buildTinyGoIntegrationGuest(t)
	program, err := capcompute.NewProgram[integrationPID](ctx, capcompute.Config{
		Image: extism.Manifest{
			Wasm: []extism.Wasm{extism.WasmFile{Path: wasmPath}},
		},
		PluginConfig: extism.PluginConfig{EnableWasi: true},
	})
	if err != nil {
		t.Fatalf("new program: %v", err)
	}
	t.Cleanup(func() {
		if err := program.Close(context.Background()); err != nil {
			t.Errorf("close program: %v", err)
		}
	})

	observe := func(id string) string {
		pid := integrationPID{id: id}
		process, err := capcompute.NewProcess(ctx, program, capcompute.ProcessSpec[integrationPID]{
			Input:      []byte(`{"mode":"ambient"}`),
			Entrypoint: "run",
			Cred:       pid,
			Dispatcher: integrationDispatcher{},
		})
		if err != nil {
			t.Fatalf("create process: %v", err)
		}
		t.Cleanup(func() {
			if err := process.Close(context.Background()); err != nil {
				t.Errorf("close process: %v", err)
			}
		})
		handle, err := capcompute.Resume(ctx, process)
		if err != nil {
			t.Fatalf("resume: %v", err)
		}
		result := <-handle.Results()
		if result.Status != capcompute.ResumeCompleted {
			t.Fatalf("status = %s; err = %v; output = %s", result.Status, result.Err, result.Output)
		}
		return string(result.Output)
	}

	first := observe("ambient-1")
	second := observe("ambient-2")
	if first != second {
		t.Fatalf("ambient reads diverged across fresh processes:\n%s\n%s", first, second)
	}
}

// Kernel law #1 (no ambient authority): a guest attempting network access
// through extism:host/env http_request — bypassing the syscall dispatcher —
// must fail, because the kernel refuses images that set allowed_hosts and the
// SDK denies requests when none are allowed.
func TestTinyGoGuestAmbientHTTPIsDenied(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping TinyGo integration test in short mode")
	}
	if _, err := exec.LookPath("tinygo"); err != nil {
		t.Skip("tinygo not found")
	}

	ctx := context.Background()
	wasmPath := buildTinyGoIntegrationGuest(t)
	program, err := capcompute.NewProgram[integrationPID](ctx, capcompute.Config{
		Image: extism.Manifest{
			Wasm: []extism.Wasm{extism.WasmFile{Path: wasmPath}},
		},
		PluginConfig: extism.PluginConfig{EnableWasi: true},
	})
	if err != nil {
		t.Fatalf("new program: %v", err)
	}
	t.Cleanup(func() {
		if err := program.Close(context.Background()); err != nil {
			t.Errorf("close program: %v", err)
		}
	})

	pid := integrationPID{id: "ambient-http"}
	process, err := capcompute.NewProcess(ctx, program, capcompute.ProcessSpec[integrationPID]{
		Input:      []byte(`{"mode":"http"}`),
		Entrypoint: "run",
		Cred:       pid,
		Dispatcher: integrationDispatcher{},
	})
	if err != nil {
		t.Fatalf("create process: %v", err)
	}
	t.Cleanup(func() {
		if err := process.Close(context.Background()); err != nil {
			t.Errorf("close process: %v", err)
		}
	})
	handle, err := capcompute.Resume(ctx, process)
	if err != nil {
		t.Fatalf("resume: %v", err)
	}
	result := <-handle.Results()
	if result.Status != capcompute.ResumeFailed {
		t.Fatalf("ambient http produced status %s (output %s); want failed", result.Status, result.Output)
	}
}

func TestTinyGoGuestResourceLimits(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping TinyGo integration test in short mode")
	}
	if _, err := exec.LookPath("tinygo"); err != nil {
		t.Skip("tinygo not found")
	}

	ctx := context.Background()
	wasmPath := buildTinyGoIntegrationGuest(t)

	newProgram := func(t *testing.T, config capcompute.Config) *capcompute.Program[integrationPID] {
		t.Helper()
		config.Image = extism.Manifest{Wasm: []extism.Wasm{extism.WasmFile{Path: wasmPath}}}
		config.PluginConfig = extism.PluginConfig{EnableWasi: true}
		program, err := capcompute.NewProgram[integrationPID](ctx, config)
		if err != nil {
			t.Fatalf("new program: %v", err)
		}
		t.Cleanup(func() {
			if err := program.Close(context.Background()); err != nil {
				t.Errorf("close program: %v", err)
			}
		})
		return program
	}

	resume := func(t *testing.T, program *capcompute.Program[integrationPID], mode string, timeout time.Duration) capcompute.ResumeResult {
		t.Helper()
		pid := integrationPID{id: "run-" + mode}
		input, err := json.Marshal(struct {
			Mode string `json:"mode"`
		}{Mode: mode})
		if err != nil {
			t.Fatalf("encode input: %v", err)
		}
		process, err := capcompute.NewProcess(ctx, program, capcompute.ProcessSpec[integrationPID]{
			Input:         input,
			Entrypoint:    "run",
			Cred:          pid,
			Dispatcher:    integrationDispatcher{},
			ResumeTimeout: timeout,
		})
		if err != nil {
			t.Fatalf("create process: %v", err)
		}
		t.Cleanup(func() {
			if err := process.Close(context.Background()); err != nil && !errors.Is(err, capcompute.ErrProcessActive) {
				t.Errorf("close process: %v", err)
			}
		})
		handle, err := capcompute.Resume(ctx, process)
		if err != nil {
			t.Fatalf("resume: %v", err)
		}
		select {
		case result := <-handle.Results():
			return result
		case <-time.After(30 * time.Second):
			t.Fatal("resume did not return")
			panic("unreachable")
		}
	}

	t.Run("memory cap traps the hog", func(t *testing.T) {
		program := newProgram(t, capcompute.Config{
			MaxMemoryPages: 256, // 16 MiB
		})
		result := resume(t, program, "hog", 0)
		if result.Status != capcompute.ResumeFailed {
			t.Fatalf("status = %s, want %s; err = %v", result.Status, capcompute.ResumeFailed, result.Err)
		}
		if result.Err == nil {
			t.Fatal("failed resume returned nil error")
		}
	})

	t.Run("deadline stops the infinite loop", func(t *testing.T) {
		program := newProgram(t, capcompute.Config{})
		result := resume(t, program, "infinite", time.Second)
		if result.Status != capcompute.ResumeStopped {
			t.Fatalf("status = %s, want %s; err = %v", result.Status, capcompute.ResumeStopped, result.Err)
		}
		if !errors.Is(result.Err, context.DeadlineExceeded) {
			t.Fatalf("error = %v, want context deadline exceeded", result.Err)
		}
	})
}
