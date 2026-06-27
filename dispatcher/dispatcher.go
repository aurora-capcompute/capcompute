// Package dispatcher defines the vocabulary a guest host call speaks: a Call
// from the guest, an Outcome (result, yield, or failure) back, and the
// Dispatcher interface that turns one into the other. It owns those types and
// the small WithCapabilities decorator that advertises a dispatcher's
// operations; it owns no capability behavior, persistence, or replay policy —
// those live in concrete dispatchers and the replay decorators above it.
package dispatcher

import (
	"context"
	"encoding/json"
)

// Dispatcher owns policy and handler dispatch for new guest calls.
type Dispatcher[K any] interface {
	Dispatch(ctx context.Context, guestData K, call Call) (Outcome, error)
}

// Capability describes one guest-callable operation exposed by a dispatcher.
type Capability struct {
	Name        string          `json:"name"`
	Description string          `json:"description"`
	InputSchema json.RawMessage `json:"input_schema"`
}

// CapabilityProvider optionally exposes the operations accepted by a dispatcher.
type CapabilityProvider interface {
	Capabilities() []Capability
}

// Capabilities returns a defensive copy of capabilities exposed by dispatcher.
func Capabilities[K any](value Dispatcher[K]) []Capability {
	provider, ok := value.(CapabilityProvider)
	if !ok {
		return nil
	}
	return cloneCapabilities(provider.Capabilities())
}
