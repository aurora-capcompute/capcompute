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
	// Hidden keeps a capability dispatchable but excluded from the program's
	// discoverable tool menu (e.g. the LLM cognition tool the program calls by a
	// name it already knows).
	Hidden bool `json:"hidden,omitempty"`
	// Labels are the source classes this capability's results carry (e.g.
	// "untrusted_web", "secret"). The provenance monitor stamps them onto
	// every result and journals them — taint tracking starts here.
	Labels []string `json:"labels,omitempty"`
	// Forbid lists labels that may not flow into this capability's args
	// (e.g. a destructive capability forbids "untrusted_web"). Because the
	// guest is opaque, flow is judged conservatively: once a process has observed
	// a label, everything it emits may derive from it.
	Forbid []string `json:"forbid,omitempty"`

	// Discriminator names the argument property that selects which Operation a
	// call is (e.g. "operation", "method"). Empty means the capability is one
	// operation and Operations is empty. It is read, never canonicalized: the
	// value the guest sent is matched exactly, so a reference monitor above the
	// journal and a dispatcher below it always resolve the same case.
	Discriminator string `json:"discriminator,omitempty"`
	// Operations are the cases of this capability's ADT, each with its own
	// argument shape and its own flow policy. A capability that declares them
	// carries the policy here rather than at the capability level, so a monitor
	// can enforce per-case instead of per-family.
	Operations []Operation `json:"operations,omitempty"`
}

// Operation is one case of a capability's ADT: what it is, the shape of its
// arguments, and the flow and approval policy that applies to it alone.
type Operation struct {
	Name        string          `json:"name"`
	Description string          `json:"description,omitempty"`
	InputSchema json.RawMessage `json:"input_schema,omitempty"`
	// Labels are the source classes this operation's results carry; Forbid are
	// the labels that may not flow into its arguments. Both mean exactly what
	// their capability-level counterparts mean, scoped to one case.
	Labels []string `json:"labels,omitempty"`
	Forbid []string `json:"forbid,omitempty"`
	// RequireApproval parks the call as a durable task until a human resolves
	// it, rather than serving it.
	RequireApproval bool `json:"require_approval,omitempty"`
}

// FindOperation resolves the case a call belongs to. It returns false when the
// capability has no ADT, when the discriminator is absent or not a string, or
// when the value names no declared operation — every one of which must be
// treated as "no policy resolved", never as "no policy applies".
func (c Capability) FindOperation(args json.RawMessage) (Operation, bool) {
	if c.Discriminator == "" || len(c.Operations) == 0 {
		return Operation{}, false
	}
	var envelope map[string]json.RawMessage
	if err := json.Unmarshal(args, &envelope); err != nil {
		return Operation{}, false
	}
	raw, ok := envelope[c.Discriminator]
	if !ok {
		return Operation{}, false
	}
	var value string
	if err := json.Unmarshal(raw, &value); err != nil {
		return Operation{}, false
	}
	for _, operation := range c.Operations {
		if operation.Name == value {
			return operation, true
		}
	}
	return Operation{}, false
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
// the process — never guest-supplied), syscall is *what* is being asked, and auth
// is *what has been granted* for this specific call (the resolved approval
// context). Leaf drivers that only perform work should ignore cred; only
// policy decorators (validation, approval, quotas) consume it.
type Dispatcher[K any] interface {
	Dispatch(ctx context.Context, cred K, syscall Syscall, auth Authorization) (SyscallResult, error)
	Capabilities() []Capability
}

// FindCapability resolves a capability by name in a grant set. Every monitor
// layer (validation, flow policy, labeling, delegation) answers the same
// question — "what does this name mean in this grant set?" — so it lives here.
func FindCapability(grants []Capability, name string) (Capability, bool) {
	for _, capability := range grants {
		if capability.Name == name {
			return capability, true
		}
	}
	return Capability{}, false
}
