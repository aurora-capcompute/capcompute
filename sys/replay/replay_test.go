package replay

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/aurora-capcompute/capcompute/sys"
)

type tapeFunc struct {
	nextResult sys.SyscallResult
	nextOK     bool
	reset      bool
	recorded   bool
}

func (t *tapeFunc) Next(sys.Syscall) (sys.SyscallResult, bool, error) {
	return t.nextResult, t.nextOK, nil
}

func (t *tapeFunc) Record(sys.Syscall, sys.SyscallResult) error {
	t.recorded = true
	return nil
}

func (t *tapeFunc) Reset() {
	t.reset = true
}

func (t *tapeFunc) Remaining() int {
	return 0
}

type nextFunc[K any] func(context.Context, K, sys.Syscall, sys.Authorization) (sys.SyscallResult, error)

func (f nextFunc[K]) Dispatch(ctx context.Context, cred K, syscall sys.Syscall, auth sys.Authorization) (sys.SyscallResult, error) {
	return f(ctx, cred, syscall, auth)
}

func (nextFunc[K]) Capabilities() []sys.Capability { return nil }

func TestDispatcherResetsTapeOnYield(t *testing.T) {
	tape := &tapeFunc{}
	replay := &Dispatcher[string]{
		tape: tape,
		next: nextFunc[string](func(context.Context, string, sys.Syscall, sys.Authorization) (sys.SyscallResult, error) {
			return sys.Yield("waiting"), nil
		}),
	}

	result, err := replay.Dispatch(context.Background(), "run-1", sys.Syscall{Name: "step.one"}, sys.Authorization{})
	if err != nil {
		t.Fatalf("dispatch: %v", err)
	}
	if result.Status() != sys.StatusYield {
		t.Fatalf("result = %#v", result)
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
		next: nextFunc[string](func(context.Context, string, sys.Syscall, sys.Authorization) (sys.SyscallResult, error) {
			return sys.Result(json.RawMessage(`{"ok":true}`)), nil
		}),
	}

	result, err := replay.Dispatch(context.Background(), "run-1", sys.Syscall{Name: "step.one"}, sys.Authorization{})
	if err != nil {
		t.Fatalf("dispatch: %v", err)
	}
	if result.Status() != sys.StatusResult {
		t.Fatalf("result = %#v", result)
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
		next: nextFunc[string](func(context.Context, string, sys.Syscall, sys.Authorization) (sys.SyscallResult, error) {
			return sys.Fail("denied"), nil
		}),
	}

	result, err := replay.Dispatch(context.Background(), "run-1", sys.Syscall{Name: "step.one"}, sys.Authorization{})
	if err != nil {
		t.Fatalf("dispatch: %v", err)
	}
	if result.Status() != sys.StatusFailed {
		t.Fatalf("result = %#v", result)
	}
	if tape.reset {
		t.Fatal("failure should not reset tape")
	}
	if !tape.recorded {
		t.Fatal("failure should be recorded")
	}
}
