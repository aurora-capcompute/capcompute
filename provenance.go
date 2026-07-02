package capcompute

import (
	"context"
	"encoding/json"
	"fmt"
	"sort"
	"strings"
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
	case sys.SyscallBegin, sys.SyscallCommit, sys.SyscallDeclassify:
		return result, nil // kernel control syscalls carry no data provenance
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

	// Hand the run's taint downstream: drivers that store guest-derived data
	// (tenant memory) persist it with the value, so provenance survives into
	// later threads instead of being laundered.
	result, err := m.next.Dispatch(sys.WithTaint(ctx, m.taintOf(pid)), cred, syscall, auth)
	if err != nil {
		return result, err
	}
	if syscall.Name == sys.SyscallDeclassify && result.Status() == sys.StatusResult {
		// The approved, journaled crossing — fresh or replayed — lifts the
		// labels instead of accumulating them.
		var crossed DeclassifyResult
		if err := json.Unmarshal(result.Result(), &crossed); err != nil {
			return sys.SyscallResult{}, fmt.Errorf("decode declassify result: %w", err)
		}
		m.Declassify(pid, crossed.Declassified...)
		return result, nil
	}
	m.observe(pid, result.Labels())
	return result, nil
}

func (m *FlowMonitor[ID, K]) Capabilities() []sys.Capability {
	return m.next.Capabilities()
}

// Declassify removes labels from a run's accumulated taint — an explicit,
// governed crossing of a label boundary (DIFC declassification). Guests reach
// it through the sys.declassify syscall (the Declassifier decorator), whose
// approved result replays through Dispatch and lands here; the direct method
// remains for host-side administrative use.
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

func (m *FlowMonitor[ID, K]) taintOf(pid ID) []string {
	m.mu.Lock()
	defer m.mu.Unlock()
	taint := m.observed[pid]
	labels := make([]string, 0, len(taint))
	for label := range taint {
		labels = append(labels, label)
	}
	sort.Strings(labels)
	return labels
}

// DeclassifyRequest is the args payload of the reserved sys.declassify
// syscall: which labels to lift from the run's taint, and why — the reason is
// required because it is what the approving human reads and what the journal
// preserves.
type DeclassifyRequest struct {
	Labels []string `json:"labels"`
	Reason string   `json:"reason"`
}

// DeclassifyResult is the journaled outcome of an approved declassification.
type DeclassifyResult struct {
	Declassified []string `json:"declassified"`
}

var declassifyInputSchema = json.RawMessage(`{
	"type": "object",
	"required": ["labels", "reason"],
	"properties": {
		"labels": {"type": "array", "items": {"type": "string", "minLength": 1}, "minItems": 1},
		"reason": {"type": "string", "minLength": 1}
	},
	"additionalProperties": false
}`)

// Declassifier serves the reserved sys.declassify syscall — DIFC
// declassification as a governed operation. Every crossing requires a human
// approval (there is no unapproved path: an unattended declassify would just
// be flow-policy bypass), and the approved crossing is a journaled syscall
// result, so replay re-applies it without asking again. It must sit *below*
// the replay layer; the FlowMonitor above performs the actual taint removal
// when the result passes through it.
type Declassifier[K any] struct {
	next sys.Dispatcher[K]
}

func NewDeclassifier[K any](next sys.Dispatcher[K]) *Declassifier[K] {
	return &Declassifier[K]{next: next}
}

func (d *Declassifier[K]) Dispatch(ctx context.Context, cred K, syscall sys.Syscall, auth sys.Authorization) (sys.SyscallResult, error) {
	if syscall.Name != sys.SyscallDeclassify {
		return d.next.Dispatch(ctx, cred, syscall, auth)
	}
	var request DeclassifyRequest
	if err := json.Unmarshal(syscall.Args, &request); err != nil {
		return sys.FailCode(sys.ErrnoInvalidArgs, fmt.Sprintf("decode declassify args: %v", err)), nil
	}
	if len(request.Labels) == 0 {
		return sys.FailCode(sys.ErrnoInvalidArgs, "declassify: labels are required"), nil
	}
	if strings.TrimSpace(request.Reason) == "" {
		return sys.FailCode(sys.ErrnoInvalidArgs, "declassify: a reason is required"), nil
	}
	if auth.Decision != sys.Approved {
		return sys.Yield(fmt.Sprintf("Approve declassifying %v: %s", request.Labels, request.Reason)), nil
	}

	crossed := append([]string(nil), request.Labels...)
	sort.Strings(crossed)
	result, err := json.Marshal(DeclassifyResult{Declassified: crossed})
	if err != nil {
		return sys.SyscallResult{}, err
	}
	return sys.Result(result), nil
}

// Capabilities publishes sys.declassify (schema'd) alongside the chain's own
// capabilities; whether a given run may call it is the manifest's decision,
// enforced by the Validator's grant set like any capability.
func (d *Declassifier[K]) Capabilities() []sys.Capability {
	return append(d.next.Capabilities(), sys.Capability{
		Name:        sys.SyscallDeclassify,
		Description: "lift labels from this run's taint after human approval; the crossing is journaled with its reason",
		InputSchema: declassifyInputSchema,
	})
}
