package capcompute_test

// End-to-end adversarial proofs. Unlike integration_tinygo_test.go (which needs
// TinyGo and therefore skips in most CI), these build a hostile guest with the
// *standard* Go toolchain (GOOS=wasip1) — the same `go` that runs the tests — so
// the guarantees below are exercised everywhere `go test` runs, never silently
// skipped.
//
// Two properties are proven through the real seams, not decorator doubles:
//
//   - Host isolation (kernel law #1): a guest driven through the real kernel
//     host function cannot read/write the host filesystem, read host env/args,
//     reach the network, exhaust host memory, spin forever, or crash the host
//     by forcing a dispatcher error.
//   - Complete mediation (kernel law #4): a guest driven through the real
//     capcompute.Stack chain (Validator → FlowMonitor → replay → Labeler →
//     Declassifier → driver) cannot invoke an ungranted capability, pass args
//     that violate a capability schema, forge the ABI, or move tainted data
//     into a forbidden sink. Every denial is observed *by the guest*, across
//     the actual trap boundary.

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
	"github.com/aurora-capcompute/capcompute/sys/replay/tape/journaled"
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

// --- in-memory journal (external-package copy of the kernel's test double) ---

type advJournal struct {
	header    journaled.Header
	hasHeader bool
	records   []journaled.Record
}

func (j *advJournal) Header() (journaled.Header, bool, error) { return j.header, j.hasHeader, nil }
func (j *advJournal) SetHeader(h journaled.Header) error      { j.header, j.hasHeader = h, true; return nil }
func (j *advJournal) Append(r journaled.Record) error         { j.records = append(j.records, r); return nil }
func (j *advJournal) Length() int                             { return len(j.records) }
func (j *advJournal) Load(idx int) (journaled.Record, error) {
	if idx < 0 || idx >= len(j.records) {
		return journaled.Record{}, errors.New("no record")
	}
	return j.records[idx], nil
}

// --- process table ---

type advTable struct {
	mu        sync.Mutex
	processes map[string]*capcompute.Process[advPID]
}

func newAdvTable() *advTable {
	return &advTable{processes: make(map[string]*capcompute.Process[advPID])}
}

func (t *advTable) LoadProcess(_ context.Context, pid string) (*capcompute.Process[advPID], error) {
	t.mu.Lock()
	defer t.mu.Unlock()
	p, ok := t.processes[pid]
	if !ok {
		return nil, capcompute.ErrProcessRequired
	}
	return p, nil
}

func (t *advTable) SaveProcess(_ context.Context, pid string, p *capcompute.Process[advPID]) error {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.processes[pid] = p
	return nil
}

// --- drivers ---

// mediationCaps is the granted surface the mediation tests run against. The
// grant set handed to the Validator is exactly this, so a name not listed here
// is ungranted by construction.
var mediationCaps = []sys.Capability{
	{Name: "host.echo"},
	{Name: "host.boom"}, // granted, but its driver returns an infrastructure error
	{Name: "internet.read", Labels: []string{"untrusted_web"}},
	{Name: "k8s.delete", Forbid: []string{"untrusted_web"}},
	{Name: "mail.send", InputSchema: json.RawMessage(`{"type":"object","required":["to"],"properties":{"to":{"type":"string"}}}`)},
}

// mediationDriver is the leaf. It serves the granted capabilities and returns a
// Go error for host.boom (an infrastructure failure the guest must never see).
type mediationDriver struct{}

func (mediationDriver) Dispatch(_ context.Context, _ advPID, sc sys.Syscall, _ sys.Authorization) (sys.SyscallResult, error) {
	switch sc.Name {
	case "host.boom":
		return sys.SyscallResult{}, errors.New("driver boom: infrastructure failure")
	default:
		return sys.Result(json.RawMessage(`{"ok":true}`)), nil
	}
}

func (mediationDriver) Capabilities() []sys.Capability { return mediationCaps }

// bareEcho is a minimal driver used for host-isolation modes that do not need
// the mediation chain. host.boom errors; everything else echoes.
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
	stack    bool                   // wrap driver in the real capcompute.Stack
	noSave   bool                   // skip saving the process (to prove not_found)
}

func runAdversary(t *testing.T, mode string, s advSetup) (capcompute.ResumeResult[advPID], *advReport) {
	t.Helper()
	ctx := context.Background()
	wasm := adversaryWasm(t)
	table := newAdvTable()

	kernel, err := capcompute.NewKernel[string, advPID](ctx, capcompute.Config[string, advPID]{
		Image:          extism.Manifest{Wasm: []extism.Wasm{extism.WasmFile{Path: wasm}}},
		PluginConfig:   extism.PluginConfig{EnableWasi: true},
		ProcessTable:   table,
		MaxMemoryPages: s.memPages,
		ResumeTimeout:  s.timeout,
	})
	if err != nil {
		t.Fatalf("new kernel: %v", err)
	}
	t.Cleanup(func() { _ = kernel.Shutdown(context.Background()) })

	driver := s.driver
	if driver == nil {
		driver = bareEcho{}
	}
	dispatcher := driver
	if s.stack {
		journal := &advJournal{}
		tape, err := journaled.NewTape(journal, journaled.Header{ABI: sys.ABIVersion, Program: "sha256:adversary", Process: "adv"})
		if err != nil {
			t.Fatalf("new tape: %v", err)
		}
		chain, err := capcompute.Stack[string, advPID]{
			Grants: func(advPID) []sys.Capability { return driver.Capabilities() },
			Taints: capcompute.NewTaints[string](),
		}.ForProcess(tape, driver)
		if err != nil {
			t.Fatalf("for process: %v", err)
		}
		dispatcher = chain
	}

	pid := advPID{id: "adv-" + mode}
	input, _ := json.Marshal(map[string]string{"mode": mode})
	process, err := kernel.CreateProcess(ctx, capcompute.ProcessSpec[string, advPID]{
		Input:      input,
		Entrypoint: "run",
		Cred:       pid,
		Dispatcher: dispatcher,
	})
	if err != nil {
		t.Fatalf("create process: %v", err)
	}
	t.Cleanup(func() {
		if err := process.Close(context.Background()); err != nil && !errors.Is(err, capcompute.ErrProcessActive) {
			t.Errorf("close process: %v", err)
		}
	})
	if !s.noSave {
		if err := table.SaveProcess(ctx, pid.PID(), process); err != nil {
			t.Fatalf("save process: %v", err)
		}
	}

	handle, err := kernel.Resume(ctx, process)
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
		return capcompute.ResumeResult[advPID]{}, nil
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
	table := newAdvTable()
	kernel, err := capcompute.NewKernel[string, advPID](ctx, capcompute.Config[string, advPID]{
		Image:        extism.Manifest{Wasm: []extism.Wasm{extism.WasmFile{Path: wasm}}},
		PluginConfig: extism.PluginConfig{EnableWasi: true},
		ProcessTable: table,
	})
	if err != nil {
		t.Fatalf("new kernel: %v", err)
	}
	t.Cleanup(func() { _ = kernel.Shutdown(context.Background()) })

	run := func(mode string) capcompute.ResumeResult[advPID] {
		pid := advPID{id: "adv-" + mode}
		input, _ := json.Marshal(map[string]string{"mode": mode})
		proc, err := kernel.CreateProcess(ctx, capcompute.ProcessSpec[string, advPID]{
			Input: input, Entrypoint: "run", Cred: pid, Dispatcher: bareEcho{},
		})
		if err != nil {
			t.Fatalf("create %s: %v", mode, err)
		}
		t.Cleanup(func() { _ = proc.Close(context.Background()) })
		if err := table.SaveProcess(ctx, pid.PID(), proc); err != nil {
			t.Fatalf("save %s: %v", mode, err)
		}
		handle, err := kernel.Resume(ctx, proc)
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

func TestAdversaryCannotBypassMediation(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping guest-build test in short mode")
	}
	stack := advSetup{driver: mediationDriver{}, stack: true}

	t.Run("ungranted capability is denied", func(t *testing.T) {
		r := completed(t, "call_ungranted", stack)
		if r.RStatus != "failed" || r.Code != string(sys.ErrnoDenied) {
			t.Fatalf("guest observed %s/%s, want failed/denied", r.RStatus, r.Code)
		}
	})

	t.Run("schema-violating args are rejected", func(t *testing.T) {
		r := completed(t, "call_badargs", stack)
		if r.RStatus != "failed" || r.Code != string(sys.ErrnoInvalidArgs) {
			t.Fatalf("guest observed %s/%s, want failed/invalid_args", r.RStatus, r.Code)
		}
	})

	t.Run("tainted data cannot flow into a forbidden sink", func(t *testing.T) {
		r := completed(t, "tainted_flow", stack)
		if r.Detail != "first=result" {
			t.Fatalf("the untrusted source should have succeeded: %s", r.Detail)
		}
		if r.RStatus != "failed" || r.Code != string(sys.ErrnoDenied) {
			t.Fatalf("guest observed %s/%s for the forbidden sink, want failed/denied", r.RStatus, r.Code)
		}
	})
}

// The guest-facing ABI trust boundary (host.go): a forged ABI version and
// non-protobuf bytes (a JSON envelope included — the decoder owns that
// refusal) must all be refused — never routed to a driver.
func TestAdversaryCannotForgeTheABI(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping guest-build test in short mode")
	}
	stack := advSetup{driver: mediationDriver{}, stack: true}
	for _, tc := range []struct {
		mode     string
		wantCode string
	}{
		{"forge_abi", string(sys.ErrnoBadABI)},
		{"forge_json", string(sys.ErrnoInvalidArgs)},
		{"forge_garbage", string(sys.ErrnoInvalidArgs)},
	} {
		t.Run(tc.mode, func(t *testing.T) {
			r := completed(t, tc.mode, stack)
			if r.RStatus != "failed" || r.Code != tc.wantCode {
				t.Fatalf("guest observed %s/%s, want failed/%s", r.RStatus, r.Code, tc.wantCode)
			}
		})
	}
}

// A syscall from a process the table does not know must be answered not_found,
// never routed to a driver.
func TestAdversarySyscallFromUnknownProcess(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping guest-build test in short mode")
	}
	r := completed(t, "probe_syscall", advSetup{driver: bareEcho{}, noSave: true})
	if r.RStatus != "failed" || r.Code != string(sys.ErrnoNotFound) {
		t.Fatalf("guest observed %s/%s, want failed/not_found", r.RStatus, r.Code)
	}
}
