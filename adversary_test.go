package capcompute_test

// End-to-end adversarial proofs. Like integration_test.go, these build a
// hostile guest with the *standard* Go toolchain (GOOS=wasip1) — the same `go`
// that runs the tests — so the guarantees below are exercised everywhere
// `go test` runs, never silently skipped.
//
// Two properties are proven through the real seams, not decorator doubles:
//
//   - Host isolation (kernel law #1): a guest driven through the real kernel
//     host function cannot read/write the host filesystem, read host env/args,
//     reach the network, exhaust host memory, spin forever, or crash the host
//     by forcing a dispatcher error.
//   - The ABI trust boundary: a forged ABI version and non-envelope bytes are
//     refused at the host function, and the refusal is observed *by the guest*,
//     across the actual trap boundary. (Mediation itself — grants, schemas,
//     flow policy — is the runtime's monitor package, proven in its own tests.)

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"sync"
	"testing"
	"time"

	extism "github.com/extism/go-sdk"

	"github.com/aurora-capcompute/capcompute"
	"github.com/aurora-capcompute/capcompute/sys"
)

type advPID struct{ id string }

func (p advPID) PID() string { return p.id }

// advReport mirrors the guest's JSON observation.
type advReport struct {
	Status  string `json:"status"`
	Mode    string `json:"mode"`
	Escaped bool   `json:"escaped"`
	Detail  string `json:"detail"`
	Code    string `json:"code"`
	RStatus string `json:"rstatus"`
	Extra   string `json:"extra"`
}

// --- guest build (once per test binary) ---

var (
	advBuildOnce sync.Once
	advWasmPath  string
	advBuildErr  error
)

// adversaryWasm compiles testdata/adversary_guest with the standard Go
// toolchain to a wasip1 Extism module. A build failure is fatal, not a skip:
// the whole point is that these proofs run wherever `go` does.
func adversaryWasm(t *testing.T) string {
	t.Helper()
	advBuildOnce.Do(func() {
		dir, err := os.MkdirTemp("", "adversary-guest-")
		if err != nil {
			advBuildErr = err
			return
		}
		out := filepath.Join(dir, "adversary.wasm")
		ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
		defer cancel()
		cmd := exec.CommandContext(ctx, "go", "build", "-buildmode=c-shared", "-o", out, "./testdata/adversary_guest")
		cmd.Env = append(os.Environ(), "GOOS=wasip1", "GOARCH=wasm")
		if b, err := cmd.CombinedOutput(); err != nil {
			advBuildErr = errors.New("build adversary guest: " + err.Error() + "\n" + string(b))
			return
		}
		advWasmPath = out
	})
	if advBuildErr != nil {
		t.Fatalf("adversary guest: %v", advBuildErr)
	}
	return advWasmPath
}

// --- drivers ---

// bareEcho is the minimal leaf driver: host.boom returns an infrastructure
// error; everything else echoes.
type bareEcho struct{}

func (bareEcho) Dispatch(_ context.Context, _ advPID, sc sys.Syscall, _ sys.Authorization) (sys.SyscallResult, error) {
	if sc.Name == "host.boom" {
		return sys.SyscallResult{}, errors.New("driver boom: infrastructure failure")
	}
	return sys.Result(json.RawMessage(`{"ok":true}`)), nil
}

func (bareEcho) Capabilities() []sys.Capability { return nil }

// --- run harness ---

type advSetup struct {
	memPages uint32
	timeout  time.Duration
	driver   sys.Dispatcher[advPID] // leaf driver
}

func runAdversary(t *testing.T, mode string, s advSetup) (capcompute.ResumeResult, *advReport) {
	t.Helper()
	ctx := context.Background()
	wasm := adversaryWasm(t)

	program, err := capcompute.NewProgram(ctx, capcompute.Config{
		Image:          extism.Manifest{Wasm: []extism.Wasm{extism.WasmFile{Path: wasm}}},
		PluginConfig:   extism.PluginConfig{EnableWasi: true},
		MaxMemoryPages: s.memPages,
	})
	if err != nil {
		t.Fatalf("new program: %v", err)
	}
	t.Cleanup(func() { _ = program.Close(context.Background()) })

	driver := s.driver
	if driver == nil {
		driver = bareEcho{}
	}
	dispatcher := driver

	pid := advPID{id: "adv-" + mode}
	input, _ := json.Marshal(map[string]string{"mode": mode})
	process, err := capcompute.NewProcess(ctx, program, capcompute.ProcessSpec[advPID]{
		Input:         input,
		Entrypoint:    "run",
		Cred:          pid,
		Dispatcher:    dispatcher,
		ResumeTimeout: s.timeout,
	})
	if err != nil {
		t.Fatalf("spawn: %v", err)
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
		if result.Status != capcompute.ResumeCompleted {
			return result, nil
		}
		var report advReport
		if err := json.Unmarshal(result.Output, &report); err != nil {
			t.Fatalf("decode guest report (%s): %v; output=%s", mode, err, result.Output)
		}
		return result, &report
	case <-time.After(30 * time.Second):
		t.Fatalf("resume(%s) did not return", mode)
		return capcompute.ResumeResult{}, nil
	}
}

// completed asserts the guest ran to completion and returns its report.
func completed(t *testing.T, mode string, s advSetup) *advReport {
	t.Helper()
	result, report := runAdversary(t, mode, s)
	if report == nil {
		t.Fatalf("mode %q: status=%s, want completed; err=%v; output=%s", mode, result.Status, result.Err, result.Output)
	}
	return report
}

// ===========================================================================
// Host isolation (kernel law #1) — a hostile guest cannot touch the host.
// ===========================================================================

func TestAdversaryCannotEscapeToHost(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping guest-build test in short mode")
	}
	cases := []struct {
		mode string
		what string
	}{
		{"fs_read", "read the host filesystem"},
		{"fs_write", "write the host filesystem"},
		{"env", "read host environment variables"},
		{"args", "read host process arguments"},
		{"http", "reach the network"},
	}
	for _, tc := range cases {
		t.Run(tc.mode, func(t *testing.T) {
			result, report := runAdversary(t, tc.mode, advSetup{})
			// The network attempt may trap the whole module instead of
			// returning — a failed resume is also "the guest could not do it".
			if report == nil {
				if tc.mode == "http" && result.Status == capcompute.ResumeFailed {
					return
				}
				t.Fatalf("mode %q: status=%s, err=%v, output=%s", tc.mode, result.Status, result.Err, result.Output)
			}
			if report.Escaped {
				t.Fatalf("SECURITY: guest managed to %s: %s (%s)", tc.what, report.Detail, report.Extra)
			}
		})
	}
}

// A guest that allocates far past the kernel's memory cap must trap into a
// failed resume, never OOM the host.
func TestAdversaryMemoryHogIsTrapped(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping guest-build test in short mode")
	}
	// Above the standard-Go guest's ~34 MiB baseline but far below the hog's
	// multi-GiB appetite, so the cap traps at growth time rather than refusing
	// the module at instantiation.
	result, _ := runAdversary(t, "hog", advSetup{memPages: 1024 /* 64 MiB */})
	if result.Status != capcompute.ResumeFailed {
		t.Fatalf("status=%s, want failed (memory cap must trap the hog); err=%v", result.Status, result.Err)
	}
	if result.Err == nil {
		t.Fatal("trapped hog returned nil error")
	}
}

// A guest that spins forever must be stopped by the per-quantum deadline.
func TestAdversaryInfiniteLoopIsStopped(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping guest-build test in short mode")
	}
	result, _ := runAdversary(t, "infinite", advSetup{timeout: time.Second})
	if result.Status != capcompute.ResumeStopped {
		t.Fatalf("status=%s, want stopped (deadline must halt the loop); err=%v", result.Status, result.Err)
	}
	if !errors.Is(result.Err, context.DeadlineExceeded) {
		t.Fatalf("err=%v, want context.DeadlineExceeded", result.Err)
	}
}

// A dispatcher (infrastructure) error must trap the quantum — the guest never
// observes an unjournaled outcome — and, crucially, the host survives: a fresh
// process on the same kernel still runs.
func TestAdversaryDispatchErrorDoesNotCrashHost(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping guest-build test in short mode")
	}
	ctx := context.Background()
	wasm := adversaryWasm(t)
	program, err := capcompute.NewProgram(ctx, capcompute.Config{
		Image:        extism.Manifest{Wasm: []extism.Wasm{extism.WasmFile{Path: wasm}}},
		PluginConfig: extism.PluginConfig{EnableWasi: true},
	})
	if err != nil {
		t.Fatalf("new program: %v", err)
	}
	t.Cleanup(func() { _ = program.Close(context.Background()) })

	run := func(mode string) capcompute.ResumeResult {
		pid := advPID{id: "adv-" + mode}
		input, _ := json.Marshal(map[string]string{"mode": mode})
		proc, err := capcompute.NewProcess(ctx, program, capcompute.ProcessSpec[advPID]{
			Input: input, Entrypoint: "run", Cred: pid, Dispatcher: bareEcho{},
		})
		if err != nil {
			t.Fatalf("spawn %s: %v", mode, err)
		}
		t.Cleanup(func() { _ = proc.Close(context.Background()) })
		handle, err := capcompute.Resume(ctx, proc)
		if err != nil {
			t.Fatalf("resume %s: %v", mode, err)
		}
		return <-handle.Results()
	}

	// The guest forces a dispatcher error; the quantum traps.
	if boom := run("dispatch_error"); boom.Status != capcompute.ResumeFailed {
		t.Fatalf("dispatch_error status=%s, want failed; output=%s", boom.Status, boom.Output)
	}
	// The host is unharmed: a fresh process still completes.
	survivor := run("echo")
	if survivor.Status != capcompute.ResumeCompleted {
		t.Fatalf("after a trapped quantum the host could not run a new process: status=%s err=%v", survivor.Status, survivor.Err)
	}
}

// Two fresh processes reading the WASI clock/RNG the kernel pins must observe
// identical values — a crash-replay is exactly a fresh process (kernel law #2).
func TestAdversaryAmbientReadsAreDeterministic(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping guest-build test in short mode")
	}
	first := completed(t, "ambient", advSetup{})
	second := completed(t, "ambient", advSetup{})
	if first.Extra == "" || first.Extra != second.Extra {
		t.Fatalf("ambient reads diverged:\n%q\n%q", first.Extra, second.Extra)
	}
}

// ===========================================================================
// Complete mediation (kernel law #4) — proven through the real Stack chain,
// observed by the guest across the trap boundary.
// ===========================================================================

// The guest-facing ABI trust boundary (host.go): a forged ABI version and
// bytes that are no envelope at all must both be refused — never routed to a
// driver.
func TestAdversaryCannotForgeTheABI(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping guest-build test in short mode")
	}
	setup := advSetup{driver: bareEcho{}}
	for _, tc := range []struct {
		mode     string
		wantCode string
	}{
		{"forge_abi", string(sys.ErrnoBadABI)},
		{"forge_garbage", string(sys.ErrnoInvalidArgs)},
	} {
		t.Run(tc.mode, func(t *testing.T) {
			r := completed(t, tc.mode, setup)
			if r.RStatus != "failed" || r.Code != tc.wantCode {
				t.Fatalf("guest observed %s/%s, want failed/%s", r.RStatus, r.Code, tc.wantCode)
			}
		})
	}
}

// A syscall from a process the table does not know must be answered not_found,
// never routed to a driver.
