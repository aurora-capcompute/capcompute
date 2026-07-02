// Package sys defines the vocabulary of the syscall boundary: a Syscall from
// the guest, a SyscallResult (result, yield, or failure) back, and the
// Dispatcher interface that turns one into the other. Authorization carries
// the forward-propagating approval context for replayed external tasks.
// This package owns no capability behavior, persistence, or replay policy —
// those live in concrete dispatchers and the replay decorators above it.
package sys

import (
	"context"
	"encoding/json"
)

type Capability struct {
	Name        string          `json:"name"`
	Description string          `json:"description"`
	InputSchema json.RawMessage `json:"input_schema"`
	// Hidden keeps a capability dispatchable but excluded from the brain's
	// discoverable tool menu (e.g. the LLM cognition tool the brain calls by a
	// name it already knows).
	Hidden bool `json:"hidden,omitempty"`
	// Compensation declares how a completed effect of this capability can be
	// undone when a run or scope aborts (saga unwinding). The zero value
	// escalates: an undeclared effect is assumed irreversible.
	Compensation Compensation `json:"compensation,omitzero"`
	// Labels are the source classes this capability's results carry (e.g.
	// "untrusted_web", "secret"). The provenance monitor stamps them onto
	// every result and journals them — taint tracking starts here.
	Labels []string `json:"labels,omitempty"`
	// Forbid lists labels that may not flow into this capability's args
	// (e.g. a destructive capability forbids "untrusted_web"). Because the
	// guest is opaque, flow is judged conservatively: once a run has observed
	// a label, everything it emits may derive from it.
	Forbid []string `json:"forbid,omitempty"`
}

// CompensationKind classifies a capability's undo story.
type CompensationKind string

const (
	// CompensateNone marks a read-only capability: nothing to undo.
	CompensateNone CompensationKind = "none"
	// CompensateSyscall names an inverse capability to dispatch. The inverse
	// receives {"compensates": <position>, "syscall": <original syscall>,
	// "result": <original result>} as its args.
	CompensateSyscall CompensationKind = "syscall"
	// CompensateEscalate (also the meaning of an undeclared, empty Kind)
	// marks an effect that cannot be undone mechanically: unwinding surfaces
	// it, with the journal, to a human — the terminal compensator.
	CompensateEscalate CompensationKind = "escalate"
)

// Compensation declares how to undo a completed effect (saga compensation,
// Garcia-Molina 1987). It is capability metadata: policy decorators and the
// kernel's unwinder consume it; leaf drivers only implement the inverse
// capability it names.
type Compensation struct {
	Kind    CompensationKind `json:"kind,omitempty"`
	Syscall string           `json:"syscall,omitempty"`
}

// Decision is the outcome of an external (human-in-the-loop) task approval.
type Decision string

const (
	Approved  Decision = "approved"
	Completed Decision = "completed"
	Failed    Decision = "failed"
	Denied    Decision = "denied"
	Cancelled Decision = "cancelled"
)

// Authorization is the forward-propagating security context for a replayed
// external task. When the runtime replays an approved task it populates this
// value and passes it to every Dispatch call; on a fresh syscall it is zero.
type Authorization struct {
	Decision Decision        `json:"decision,omitempty"`
	Data     json.RawMessage `json:"data,omitempty"`
	Actor    string          `json:"actor,omitempty"`
	Reason   string          `json:"reason,omitempty"`
}

// Dispatcher owns policy and handler dispatch for guest syscalls.
//
// The syscall triad: cred is *who* is calling (the host-side credential for
// the run — never guest-supplied), syscall is *what* is being asked, and auth
// is *what has been granted* for this specific call (the resolved approval
// context). Leaf drivers that only perform work should ignore cred; only
// policy decorators (validation, approval, quotas) consume it.
type Dispatcher[K any] interface {
	Dispatch(ctx context.Context, cred K, syscall Syscall, auth Authorization) (SyscallResult, error)
	Capabilities() []Capability
}
