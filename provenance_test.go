package capcompute

import (
	"context"
	"encoding/json"
	"slices"
	"testing"

	"github.com/aurora-capcompute/capcompute/sys"
	"github.com/aurora-capcompute/capcompute/sys/replay"
	"github.com/aurora-capcompute/capcompute/sys/replay/tape/journaled"
)

var flowCaps = []sys.Capability{
	{Name: "internet.read", Labels: []string{"untrusted_web"}},
	{Name: "k8s.delete", Forbid: []string{"untrusted_web"}},
	{Name: "mail.send"},
}

type flowDriver struct{}

func (flowDriver) Dispatch(_ context.Context, _ testPID, syscall sys.Syscall, _ sys.Authorization) (sys.SyscallResult, error) {
	return sys.Result(json.RawMessage(`{"from":"` + syscall.Name + `"}`)), nil
}

func (flowDriver) Capabilities() []sys.Capability { return flowCaps }

func call(t *testing.T, d sys.Dispatcher[testPID], pid string, name string) sys.SyscallResult {
	t.Helper()
	result, err := d.Dispatch(context.Background(), testPID{id: pid}, sys.Syscall{Abi: sys.ABIVersion, Name: name}, sys.Authorization{})
	if err != nil {
		t.Fatalf("dispatch %s: %v", name, err)
	}
	return result
}

func TestLabelerStampsDerivingCapabilityAndClasses(t *testing.T) {
	labeler := NewLabeler[testPID](flowDriver{})
	result := call(t, labeler, "p1", "internet.read")

	labels := result.Labels()
	for _, want := range []string{"syscall:internet.read", "untrusted_web"} {
		if !slices.Contains(labels, want) {
			t.Fatalf("labels = %v, want %q present", labels, want)
		}
	}
}

func TestFlowMonitorBlocksTaintedFlow(t *testing.T) {
	monitor := NewFlowMonitor[string](NewLabeler[testPID](flowDriver{}))

	// Before any taint the protected capability is callable.
	if result := call(t, monitor, "p1", "k8s.delete"); result.Status() != sys.StatusResult {
		t.Fatalf("untainted k8s.delete = %#v, want result", result)
	}

	// Observing untrusted data taints the run…
	call(t, monitor, "p1", "internet.read")

	// …so the protected capability is now refused…
	result := call(t, monitor, "p1", "k8s.delete")
	if result.Status() != sys.StatusFailed || result.Errno() != sys.ErrnoDenied {
		t.Fatalf("tainted k8s.delete = %#v, want failed/denied", result)
	}

	// …while unprotected capabilities still work…
	if result := call(t, monitor, "p1", "mail.send"); result.Status() != sys.StatusResult {
		t.Fatalf("mail.send = %#v, want result", result)
	}

	// …and other runs are unaffected.
	if result := call(t, monitor, "p2", "k8s.delete"); result.Status() != sys.StatusResult {
		t.Fatalf("other run's k8s.delete = %#v, want result", result)
	}
}

func TestFlowMonitorDeclassify(t *testing.T) {
	monitor := NewFlowMonitor[string](NewLabeler[testPID](flowDriver{}))
	call(t, monitor, "p1", "internet.read")

	if result := call(t, monitor, "p1", "k8s.delete"); result.Errno() != sys.ErrnoDenied {
		t.Fatalf("expected denial before declassification, got %#v", result)
	}
	monitor.Declassify("p1", "untrusted_web")
	if result := call(t, monitor, "p1", "k8s.delete"); result.Status() != sys.StatusResult {
		t.Fatalf("declassified k8s.delete = %#v, want result", result)
	}
}

// The full chain: FlowMonitor → replay → Labeler → driver. Labels must land in
// the journal, and a crash-restarted host must rebuild the run's taint from
// replayed results alone.
func TestFlowTaintSurvivesCrashReplay(t *testing.T) {
	journal := &memJournal{}
	header := journaled.Header{ABI: sys.ABIVersion, Program: "sha256:test", Run: "p1"}

	newChain := func(t *testing.T) *FlowMonitor[string, testPID] {
		t.Helper()
		tape, err := journaled.NewTape(journal, header)
		if err != nil {
			t.Fatalf("new tape: %v", err)
		}
		return NewFlowMonitor[string](replay.NewDispatcher[testPID](tape, NewLabeler[testPID](flowDriver{})))
	}

	first := newChain(t)
	call(t, first, "p1", "internet.read")

	// Labels reached the journal's completion record.
	completion, err := journal.Load(1)
	if err != nil || completion.Result == nil {
		t.Fatalf("completion record: %+v, err %v", completion, err)
	}
	if !slices.Contains(completion.Result.Labels(), "untrusted_web") {
		t.Fatalf("journaled labels = %v, want untrusted_web", completion.Result.Labels())
	}

	// Crash: a fresh host process (fresh monitor, fresh tape, same journal)
	// replays the run. The replayed result must rebuild the taint…
	crashed := newChain(t)
	replayed := call(t, crashed, "p1", "internet.read")
	if !slices.Contains(replayed.Labels(), "untrusted_web") {
		t.Fatalf("replayed labels = %v, want untrusted_web", replayed.Labels())
	}
	// …so the flow policy still holds after the crash.
	result := call(t, crashed, "p1", "k8s.delete")
	if result.Status() != sys.StatusFailed || result.Errno() != sys.ErrnoDenied {
		t.Fatalf("post-crash k8s.delete = %#v, want failed/denied", result)
	}
}
