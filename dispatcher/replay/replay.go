// Package replay decorates a dispatcher so a re-run guest sees the same
// outcomes it saw the first time: each call is served from a Tape if already
// recorded, otherwise delegated to the underlying dispatcher and recorded. It
// owns the replay-cursor protocol (the Tape interface) and the divergence
// checks; it does not own where the tape is stored — a concrete Tape such as
// the journaled package supplies that.
package replay

import (
	dispatcher2 "capcompute/dispatcher"
	"context"
	"fmt"
)

// CompletionChecker validates per-play dispatcher state after the guest returns.
type CompletionChecker interface {
	CheckCompleted() error
}

// Tape owns replay cursor state and decides how newly observed outcomes are stored.
type Tape interface {
	Next(call dispatcher2.Call) (dispatcher2.Outcome, bool, error)
	Record(call dispatcher2.Call, outcome dispatcher2.Outcome) error
	Reset()
	Remaining() int
}

// Dispatcher serves recorded outcomes before delegating new calls.
type Dispatcher[K any] struct {
	tape Tape
	next dispatcher2.Dispatcher[K]
}

func NewDispatcher[K any](tape Tape, next dispatcher2.Dispatcher[K]) *Dispatcher[K] {
	return &Dispatcher[K]{tape: tape, next: next}
}

func (d *Dispatcher[K]) Dispatch(ctx context.Context, key K, call dispatcher2.Call) (dispatcher2.Outcome, error) {
	outcome, replayed, err := d.tape.Next(call)
	if err != nil || replayed {
		return outcome, err
	}
	outcome, err = d.next.Dispatch(ctx, key, call)
	if err != nil {
		return dispatcher2.Outcome{}, err
	}
	if outcome.Kind() == dispatcher2.OutcomeYield {
		d.tape.Reset()
		return outcome, nil
	}
	if err := d.tape.Record(call, outcome); err != nil {
		return dispatcher2.Outcome{}, err
	}
	return outcome, nil
}

func (d *Dispatcher[K]) Remaining() int {
	return d.tape.Remaining()
}

func (d *Dispatcher[K]) CheckCompleted() error {
	if remaining := d.Remaining(); remaining > 0 {
		return IncompleteError{Remaining: remaining}
	}
	return nil
}

func (d *Dispatcher[K]) Capabilities() []dispatcher2.Capability {
	return dispatcher2.Capabilities(d.next)
}

// IncompleteError means the guest completed before replaying all recorded calls.
type IncompleteError struct {
	Remaining int
}

func (e IncompleteError) Error() string {
	return fmt.Sprintf("replay incomplete: %d recorded calls were not consumed", e.Remaining)
}
