package replay

import (
	"github.com/aurora-capcompute/capcompute/dispatcher"
	"context"
	"encoding/json"
	"testing"
)

type tapeFunc struct {
	nextOutcome dispatcher.Outcome
	nextOK      bool
	reset       bool
	recorded    bool
}

func (t *tapeFunc) Next(dispatcher.Call) (dispatcher.Outcome, bool, error) {
	return t.nextOutcome, t.nextOK, nil
}

func (t *tapeFunc) Record(dispatcher.Call, dispatcher.Outcome) error {
	t.recorded = true
	return nil
}

func (t *tapeFunc) Reset() {
	t.reset = true
}

func (t *tapeFunc) Remaining() int {
	return 0
}

type nextFunc[K any] func(context.Context, K, dispatcher.Call) (dispatcher.Outcome, error)

func (f nextFunc[K]) Dispatch(ctx context.Context, key K, call dispatcher.Call) (dispatcher.Outcome, error) {
	return f(ctx, key, call)
}

func (nextFunc[K]) Capabilities() []dispatcher.Capability { return nil }

func TestDispatcherResetsTapeOnYield(t *testing.T) {
	tape := &tapeFunc{}
	replay := &Dispatcher[string]{
		tape: tape,
		next: nextFunc[string](func(context.Context, string, dispatcher.Call) (dispatcher.Outcome, error) {
			return dispatcher.Yield("waiting"), nil
		}),
	}

	outcome, err := replay.Dispatch(context.Background(), "run-1", dispatcher.Call{Name: "step.one"})
	if err != nil {
		t.Fatalf("dispatch: %v", err)
	}
	if outcome.Kind() != dispatcher.OutcomeYield {
		t.Fatalf("outcome = %#v", outcome)
	}
	if !tape.reset {
		t.Fatal("tape was not reset")
	}
	if tape.recorded {
		t.Fatal("yield should not be recorded")
	}
}

func TestDispatcherRecordsResult(t *testing.T) {
	tape := &tapeFunc{}
	replay := &Dispatcher[string]{
		tape: tape,
		next: nextFunc[string](func(context.Context, string, dispatcher.Call) (dispatcher.Outcome, error) {
			return dispatcher.Result(json.RawMessage(`{"ok":true}`)), nil
		}),
	}

	outcome, err := replay.Dispatch(context.Background(), "run-1", dispatcher.Call{Name: "step.one"})
	if err != nil {
		t.Fatalf("dispatch: %v", err)
	}
	if outcome.Kind() != dispatcher.OutcomeResult {
		t.Fatalf("outcome = %#v", outcome)
	}
	if tape.reset {
		t.Fatal("result should not reset tape")
	}
	if !tape.recorded {
		t.Fatal("result should be recorded")
	}
}

func TestDispatcherRecordsFailure(t *testing.T) {
	tape := &tapeFunc{}
	replay := &Dispatcher[string]{
		tape: tape,
		next: nextFunc[string](func(context.Context, string, dispatcher.Call) (dispatcher.Outcome, error) {
			return dispatcher.Failed("denied"), nil
		}),
	}

	outcome, err := replay.Dispatch(context.Background(), "run-1", dispatcher.Call{Name: "step.one"})
	if err != nil {
		t.Fatalf("dispatch: %v", err)
	}
	if outcome.Kind() != dispatcher.OutcomeFailed {
		t.Fatalf("outcome = %#v", outcome)
	}
	if tape.reset {
		t.Fatal("failure should not reset tape")
	}
	if !tape.recorded {
		t.Fatal("failure should be recorded")
	}
}
