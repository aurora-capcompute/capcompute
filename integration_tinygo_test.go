package capcompute_test

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
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

// testProcessTable is an in-memory capcompute.ProcessTable. The kernel ships
// only the interface; durable tables belong to consumer modules.
type testProcessTable struct {
	mu        sync.Mutex
	processes map[string]*capcompute.Process[integrationPID]
}

func newTestProcessTable() *testProcessTable {
	return &testProcessTable{processes: make(map[string]*capcompute.Process[integrationPID])}
}

func (t *testProcessTable) LoadProcess(_ context.Context, pid string) (*capcompute.Process[integrationPID], error) {
	t.mu.Lock()
	defer t.mu.Unlock()
	process, ok := t.processes[pid]
	if !ok {
		return nil, capcompute.ErrProcessRequired
	}
	return process, nil
}

func (t *testProcessTable) SaveProcess(_ context.Context, pid string, process *capcompute.Process[integrationPID]) error {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.processes[pid] = process
	return nil
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
	table := newTestProcessTable()
	kernel, err := capcompute.NewKernel[string, integrationPID](ctx, capcompute.Config[string, integrationPID]{
		Image: extism.Manifest{
			Wasm: []extism.Wasm{extism.WasmFile{Path: wasmPath}},
		},
		PluginConfig: extism.PluginConfig{
			EnableWasi: true,
		},
		ProcessTable: table,
	})
	if err != nil {
		t.Fatalf("new kernel: %v", err)
	}
	t.Cleanup(func() {
		if err := kernel.Shutdown(context.Background()); err != nil {
			t.Errorf("shutdown kernel: %v", err)
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
			process, err := kernel.CreateProcess(ctx, capcompute.ProcessSpec[string, integrationPID]{
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

			if err := table.SaveProcess(ctx, pid.PID(), process); err != nil {
				t.Fatalf("save process: %v", err)
			}

			handle, err := kernel.Resume(ctx, process)
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

func TestTinyGoGuestCanBeStopped(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping TinyGo integration test in short mode")
	}
	if _, err := exec.LookPath("tinygo"); err != nil {
		t.Skip("tinygo not found")
	}

	ctx := context.Background()
	wasmPath := buildTinyGoIntegrationGuest(t)
	table := newTestProcessTable()
	kernel, err := capcompute.NewKernel[string, integrationPID](ctx, capcompute.Config[string, integrationPID]{
		Image: extism.Manifest{
			Wasm: []extism.Wasm{extism.WasmFile{Path: wasmPath}},
		},
		PluginConfig: extism.PluginConfig{
			EnableWasi: true,
		},
		ProcessTable: table,
	})
	if err != nil {
		t.Fatalf("new kernel: %v", err)
	}
	t.Cleanup(func() {
		if err := kernel.Shutdown(context.Background()); err != nil {
			t.Errorf("shutdown kernel: %v", err)
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
	process, err := kernel.CreateProcess(ctx, capcompute.ProcessSpec[string, integrationPID]{
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
	if err := table.SaveProcess(ctx, pid.PID(), process); err != nil {
		t.Fatalf("save process: %v", err)
	}

	handle, err := kernel.Resume(ctx, process)
	if err != nil {
		t.Fatalf("resume: %v", err)
	}
	if _, err := kernel.Resume(ctx, process); err != capcompute.ErrProcessActive {
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

	if _, err := kernel.Resume(ctx, process); err != capcompute.ErrProcessTerminated {
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
	table := newTestProcessTable()
	kernel, err := capcompute.NewKernel[string, integrationPID](ctx, capcompute.Config[string, integrationPID]{
		Image: extism.Manifest{
			Wasm: []extism.Wasm{extism.WasmFile{Path: wasmPath}},
		},
		PluginConfig: extism.PluginConfig{EnableWasi: true},
		ProcessTable: table,
	})
	if err != nil {
		t.Fatalf("new kernel: %v", err)
	}
	t.Cleanup(func() {
		if err := kernel.Shutdown(context.Background()); err != nil {
			t.Errorf("shutdown kernel: %v", err)
		}
	})

	observe := func(id string) string {
		pid := integrationPID{id: id}
		process, err := kernel.CreateProcess(ctx, capcompute.ProcessSpec[string, integrationPID]{
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
		if err := table.SaveProcess(ctx, pid.PID(), process); err != nil {
			t.Fatalf("save process: %v", err)
		}
		handle, err := kernel.Resume(ctx, process)
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
	table := newTestProcessTable()
	kernel, err := capcompute.NewKernel[string, integrationPID](ctx, capcompute.Config[string, integrationPID]{
		Image: extism.Manifest{
			Wasm: []extism.Wasm{extism.WasmFile{Path: wasmPath}},
		},
		PluginConfig: extism.PluginConfig{EnableWasi: true},
		ProcessTable: table,
	})
	if err != nil {
		t.Fatalf("new kernel: %v", err)
	}
	t.Cleanup(func() {
		if err := kernel.Shutdown(context.Background()); err != nil {
			t.Errorf("shutdown kernel: %v", err)
		}
	})

	pid := integrationPID{id: "ambient-http"}
	process, err := kernel.CreateProcess(ctx, capcompute.ProcessSpec[string, integrationPID]{
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
	if err := table.SaveProcess(ctx, pid.PID(), process); err != nil {
		t.Fatalf("save process: %v", err)
	}
	handle, err := kernel.Resume(ctx, process)
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

	newKernel := func(t *testing.T, table *testProcessTable, config capcompute.Config[string, integrationPID]) *capcompute.Kernel[string, integrationPID] {
		t.Helper()
		config.Image = extism.Manifest{Wasm: []extism.Wasm{extism.WasmFile{Path: wasmPath}}}
		config.PluginConfig = extism.PluginConfig{EnableWasi: true}
		config.ProcessTable = table
		kernel, err := capcompute.NewKernel[string, integrationPID](ctx, config)
		if err != nil {
			t.Fatalf("new kernel: %v", err)
		}
		t.Cleanup(func() {
			if err := kernel.Shutdown(context.Background()); err != nil {
				t.Errorf("shutdown kernel: %v", err)
			}
		})
		return kernel
	}

	resume := func(t *testing.T, kernel *capcompute.Kernel[string, integrationPID], table *testProcessTable, mode string) capcompute.ResumeResult[integrationPID] {
		t.Helper()
		pid := integrationPID{id: "run-" + mode}
		input, err := json.Marshal(struct {
			Mode string `json:"mode"`
		}{Mode: mode})
		if err != nil {
			t.Fatalf("encode input: %v", err)
		}
		process, err := kernel.CreateProcess(ctx, capcompute.ProcessSpec[string, integrationPID]{
			Input:      input,
			Entrypoint: "run",
			Cred:       pid,
			Dispatcher: integrationDispatcher{},
		})
		if err != nil {
			t.Fatalf("create process: %v", err)
		}
		t.Cleanup(func() {
			if err := process.Close(context.Background()); err != nil && !errors.Is(err, capcompute.ErrProcessActive) {
				t.Errorf("close process: %v", err)
			}
		})
		if err := table.SaveProcess(ctx, pid.PID(), process); err != nil {
			t.Fatalf("save process: %v", err)
		}
		handle, err := kernel.Resume(ctx, process)
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
		table := newTestProcessTable()
		kernel := newKernel(t, table, capcompute.Config[string, integrationPID]{
			MaxMemoryPages: 256, // 16 MiB
		})
		result := resume(t, kernel, table, "hog")
		if result.Status != capcompute.ResumeFailed {
			t.Fatalf("status = %s, want %s; err = %v", result.Status, capcompute.ResumeFailed, result.Err)
		}
		if result.Err == nil {
			t.Fatal("failed resume returned nil error")
		}
	})

	t.Run("deadline stops the infinite loop", func(t *testing.T) {
		table := newTestProcessTable()
		kernel := newKernel(t, table, capcompute.Config[string, integrationPID]{
			ResumeTimeout: time.Second,
		})
		result := resume(t, kernel, table, "infinite")
		if result.Status != capcompute.ResumeStopped {
			t.Fatalf("status = %s, want %s; err = %v", result.Status, capcompute.ResumeStopped, result.Err)
		}
		if !errors.Is(result.Err, context.DeadlineExceeded) {
			t.Fatalf("error = %v, want context deadline exceeded", result.Err)
		}
	})
}
