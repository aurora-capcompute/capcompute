package capcompute

import (
	"errors"
	"fmt"

	"github.com/aurora-capcompute/capcompute/sys"
	"github.com/aurora-capcompute/capcompute/sys/replay"
)

// Stack wires the kernel's canonical dispatcher chain. The order is the
// load-bearing part, so it lives in code instead of prose:
//
//	Validator → Throttle → FlowMonitor → [replay] → Labeler → Declassifier → Messenger → Spawner → drivers
//
// Above the replay layer sit the pieces whose decisions must re-derive
// deterministically on every pass and never enter the journal: validation and
// flow denials replay identically from the same grant set and taint, and the
// throttle's delays are invisible to a clockless guest. Below the replay
// layer sit the pieces whose outcomes must be journaled exactly once and
// served from the tape thereafter: stamped labels, approved declassification
// crossings, message sends/receives, and spawned child results — all of which
// also need the tape's idempotency keys. Assembling this by hand and getting
// one layer on the wrong side silently breaks a kernel law; ForRun cannot.
//
// The Stack holds the chain's cross-run components (grant source, taint
// state, rate limit, spawn/IPC config); ForRun completes it with the one
// per-run piece — the tape — and the run's drivers.
type Stack[ID comparable, K PID[ID]] struct {
	// Grants is the manifest seam: the capability set granted to a cred.
	// Required — a stack without a grant source is not a reference monitor.
	Grants GrantSource[K]
	// Taints is the shared cross-run taint state. Required — flow policy is
	// not optional in the canonical chain; grant nothing with Forbid sets and
	// it is inert.
	Taints *Taints[ID]
	// Limit enables syscall-rate throttling. Optional.
	Limit *RateLimit
	// RateKeyOf picks the throttle's accounting bucket (default: the PID, so
	// each run is limited independently; return the tenant to aggregate).
	RateKeyOf func(cred K) string
	// Spawn enables sys.spawn. Optional.
	Spawn *SpawnConfig[K]
	// IPC enables sys.send/sys.recv. Optional.
	IPC *IPCConfig[ID, K]
	// OpenIntents overrides the open-intent policy (default: retry under the
	// original idempotency key).
	OpenIntents replay.OpenIntentPolicy
}

// ForRun assembles the chain for one run around its tape and drivers.
func (s Stack[ID, K]) ForRun(tape replay.Tape, drivers sys.Dispatcher[K]) (sys.Dispatcher[K], error) {
	if s.Grants == nil {
		return nil, errors.New("stack: Grants is required")
	}
	if s.Taints == nil {
		return nil, errors.New("stack: Taints is required (share one across runs)")
	}
	if drivers == nil {
		return nil, errors.New("stack: drivers are required")
	}

	// Below the replay layer: journaled once, replayed thereafter.
	below := drivers
	if s.Spawn != nil {
		below = NewSpawner(*s.Spawn, below)
	}
	if s.IPC != nil {
		below = NewMessenger(*s.IPC, below)
	}
	below = NewLabeler[K](NewDeclassifier[K](below))

	journaled := replay.NewDispatcher(tape, below)
	if s.OpenIntents != nil {
		journaled.WithOpenIntentPolicy(s.OpenIntents)
	}

	// Above the replay layer: re-derived on every pass, never journaled.
	var above sys.Dispatcher[K] = NewFlowMonitor(s.Taints, journaled)
	if s.Limit != nil {
		keyOf := s.RateKeyOf
		if keyOf == nil {
			keyOf = func(cred K) string { return fmt.Sprint(cred.PID()) }
		}
		above = NewThrottle(s.Limit, keyOf, above)
	}
	return NewValidator(s.Grants, above), nil
}
