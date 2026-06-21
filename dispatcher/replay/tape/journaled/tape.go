package journaled

import (
	"bytes"
	"capcompute/dispatcher"
	"fmt"
)

type Tape struct {
	records Journal
	cursor  int
}

// Journal stores durable records for a tape.
type Journal interface {
	Load(idx int) (Record, error)
	Store(idx int, call dispatcher.Call, outcome dispatcher.Outcome) error
	Length() int
}

type Record struct {
	Call    dispatcher.Call
	Outcome dispatcher.Outcome
}

// ReplayDivergedError means the guest requested a different call than history recorded.
type ReplayDivergedError struct {
	Index int
	Want  dispatcher.Call
	Got   dispatcher.Call
}

func (e ReplayDivergedError) Error() string {
	return fmt.Sprintf("replay diverged at call %d: want %q got %q", e.Index, e.Want.Name, e.Got.Name)
}

// ReplayIncompleteError means the guest completed before replaying all recorded calls.
type ReplayIncompleteError struct {
	Remaining int
}

func (e ReplayIncompleteError) Error() string {
	return fmt.Sprintf("replay incomplete: %d recorded calls were not consumed", e.Remaining)
}

// NewTape creates a journal-backed replay tape whose cursor starts at the beginning.
func NewTape(journal Journal) *Tape {
	return &Tape{journal, 0}
}

// Next returns a recorded outcome for call, or ok=false when call is new.
func (t *Tape) Next(call dispatcher.Call) (dispatcher.Outcome, bool, error) {
	if t == nil || t.cursor >= t.records.Length() {
		return dispatcher.Outcome{}, false, nil
	}

	record, err := t.records.Load(t.cursor)
	if err != nil {
		return dispatcher.Outcome{}, false, err
	}
	if !sameCall(record.Call, call) {
		return dispatcher.Outcome{}, false, ReplayDivergedError{
			Index: t.cursor,
			Want:  record.Call,
			Got:   call,
		}
	}
	t.cursor++
	return record.Outcome, true, nil
}

func (t *Tape) Record(call dispatcher.Call, outcome dispatcher.Outcome) error {
	if t == nil || outcome.Kind() == dispatcher.OutcomeYield {
		return nil
	}
	if outcome.Kind() != dispatcher.OutcomeResult && outcome.Kind() != dispatcher.OutcomeFailed {
		return fmt.Errorf("cannot record invalid outcome %q", outcome.Kind())
	}
	if err := t.records.Store(t.records.Length(), call, outcome); err != nil {
		return err
	}
	t.cursor++
	return nil
}

func (t *Tape) Reset() {
	if t == nil {
		return
	}
	t.cursor = 0
}

func (t *Tape) Remaining() int {
	if t == nil {
		return 0
	}
	return t.records.Length() - t.cursor
}

func sameCall(left dispatcher.Call, right dispatcher.Call) bool {
	return left.Name == right.Name && bytes.Equal(left.Args, right.Args)
}
