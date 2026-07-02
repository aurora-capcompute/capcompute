// Package replay decorates a dispatcher so a re-run guest sees the same
// results it saw the first time: each syscall is served from a Tape if already
// recorded, otherwise delegated to the underlying dispatcher and recorded. It
// owns the replay-cursor protocol (the Tape interface) and the divergence
// checks; it does not own where the tape is stored — a concrete Tape such as
// the journaled package supplies that.
package replay

import (
	"context"
	"fmt"

	"github.com/aurora-capcompute/capcompute/sys"
)

// CompletionChecker validates per-resume dispatcher state after the guest returns.
type CompletionChecker interface {
	CheckCompleted() error
}

// Tape owns replay cursor state and decides how newly observed results are stored.
type Tape interface {
	Next(syscall sys.Syscall) (sys.SyscallResult, bool, error)
	Record(syscall sys.Syscall, result sys.SyscallResult) error
	Reset()
	Remaining() int
}

// Dispatcher serves recorded results before delegating new syscalls.
type Dispatcher[K any] struct {
	tape Tape
	next sys.Dispatcher[K]
}

func NewDispatcher[K any](tape Tape, next sys.Dispatcher[K]) *Dispatcher[K] {
	return &Dispatcher[K]{tape: tape, next: next}
}

func (d *Dispatcher[K]) Dispatch(ctx context.Context, cred K, syscall sys.Syscall, auth sys.Authorization) (sys.SyscallResult, error) {
	result, replayed, err := d.tape.Next(syscall)
	if err != nil || replayed {
		return result, err
	}
	result, err = d.next.Dispatch(ctx, cred, syscall, auth)
	if err != nil {
		return sys.SyscallResult{}, err
	}
	if result.Status() == sys.StatusYield {
		d.tape.Reset()
		return result, nil
	}
	if err := d.tape.Record(syscall, result); err != nil {
		return sys.SyscallResult{}, err
	}
	return result, nil
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

func (d *Dispatcher[K]) Capabilities() []sys.Capability {
	return d.next.Capabilities()
}

// IncompleteError means the guest completed before replaying all recorded syscalls.
type IncompleteError struct {
	Remaining int
}

func (e IncompleteError) Error() string {
	return fmt.Sprintf("replay incomplete: %d recorded syscalls were not consumed", e.Remaining)
}
