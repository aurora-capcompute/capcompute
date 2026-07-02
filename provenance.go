package capcompute

import (
	"context"
	"fmt"
	"sort"
	"sync"

	"github.com/aurora-capcompute/capcompute/sys"
)

// Labeler stamps provenance onto every result: the deriving capability
// ("syscall:<name>") plus the capability's declared source classes
// (Capability.Labels). It sits *below* the replay layer so stamped labels are
// journaled with the completion record — provenance becomes part of the audit
// trail for free.
type Labeler[K any] struct {
	next sys.Dispatcher[K]
}

func NewLabeler[K any](next sys.Dispatcher[K]) *Labeler[K] {
	return &Labeler[K]{next: next}
}

func (l *Labeler[K]) Dispatch(ctx context.Context, cred K, syscall sys.Syscall, auth sys.Authorization) (sys.SyscallResult, error) {
	result, err := l.next.Dispatch(ctx, cred, syscall, auth)
	if err != nil {
		return result, err
	}
	switch syscall.Name {
	case sys.SyscallBegin, sys.SyscallCommit:
		return result, nil // kernel markers carry no data
	}
	labels := []string{"syscall:" + syscall.Name}
	if capability, ok := findCapability(l.next.Capabilities(), syscall.Name); ok {
		labels = append(labels, capability.Labels...)
	}
	return result.WithLabels(labels...), nil
}

func (l *Labeler[K]) Capabilities() []sys.Capability {
	return l.next.Capabilities()
}

// FlowMonitor enforces information-flow policy at the reference monitor
// (the CaMeL architecture as a kernel primitive). The guest is opaque, so
// flow is judged conservatively: every label a run observes taints everything
// it later emits. A syscall to a capability whose Forbid set intersects the
// run's accumulated taint is refused with ErrnoDenied before any driver runs.
//
// It sits *above* the replay layer: replayed results flow through it, so a
// crash-restarted host rebuilds the run's taint from the journal exactly.
//
// Chain order: Validator → FlowMonitor → replay → Labeler → drivers.
type FlowMonitor[ID comparable, K PID[ID]] struct {
	next sys.Dispatcher[K]

	mu       sync.Mutex
	observed map[ID]map[string]struct{}
}

func NewFlowMonitor[ID comparable, K PID[ID]](next sys.Dispatcher[K]) *FlowMonitor[ID, K] {
	return &FlowMonitor[ID, K]{
		next:     next,
		observed: make(map[ID]map[string]struct{}),
	}
}

func (m *FlowMonitor[ID, K]) Dispatch(ctx context.Context, cred K, syscall sys.Syscall, auth sys.Authorization) (sys.SyscallResult, error) {
	switch syscall.Name {
	case sys.SyscallBegin, sys.SyscallCommit:
		return m.next.Dispatch(ctx, cred, syscall, auth)
	}

	pid := cred.PID()
	if capability, ok := findCapability(m.next.Capabilities(), syscall.Name); ok && len(capability.Forbid) > 0 {
		if tainted := m.intersection(pid, capability.Forbid); len(tainted) > 0 {
			return sys.FailCode(sys.ErrnoDenied, fmt.Sprintf(
				"flow policy: this run has observed %v, which may not flow into %q", tainted, syscall.Name)), nil
		}
	}

	result, err := m.next.Dispatch(ctx, cred, syscall, auth)
	if err != nil {
		return result, err
	}
	m.observe(pid, result.Labels())
	return result, nil
}

func (m *FlowMonitor[ID, K]) Capabilities() []sys.Capability {
	return m.next.Capabilities()
}

// Declassify removes labels from a run's accumulated taint — an explicit,
// governed crossing of a label boundary (DIFC declassification). The app owns
// when it is called; composing it with a human approval is the intended use.
func (m *FlowMonitor[ID, K]) Declassify(pid ID, labels ...string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	taint := m.observed[pid]
	for _, label := range labels {
		delete(taint, label)
	}
}

// ForgetRun releases a terminated run's taint state.
func (m *FlowMonitor[ID, K]) ForgetRun(pid ID) {
	m.mu.Lock()
	defer m.mu.Unlock()
	delete(m.observed, pid)
}

func (m *FlowMonitor[ID, K]) observe(pid ID, labels []string) {
	if len(labels) == 0 {
		return
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	taint := m.observed[pid]
	if taint == nil {
		taint = make(map[string]struct{})
		m.observed[pid] = taint
	}
	for _, label := range labels {
		taint[label] = struct{}{}
	}
}

func (m *FlowMonitor[ID, K]) intersection(pid ID, forbid []string) []string {
	m.mu.Lock()
	defer m.mu.Unlock()
	taint := m.observed[pid]
	var tainted []string
	for _, label := range forbid {
		if _, ok := taint[label]; ok {
			tainted = append(tainted, label)
		}
	}
	sort.Strings(tainted)
	return tainted
}
