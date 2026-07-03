package capcompute

import (
	"context"
	"encoding/json"
	"errors"
	"testing"

	"github.com/aurora-capcompute/capcompute/sys"
	"github.com/aurora-capcompute/capcompute/sys/replay/tape/journaled"
)

type sagaCall struct {
	Syscall sys.Syscall
	Key     string
}

type sagaDispatcher struct {
	caps    []sys.Capability
	handler func(syscall sys.Syscall) (sys.SyscallResult, error)
	calls   []sagaCall
}

func (d *sagaDispatcher) Dispatch(ctx context.Context, _ testPID, syscall sys.Syscall, _ sys.Authorization) (sys.SyscallResult, error) {
	key, _ := sys.IdempotencyKey(ctx)
	d.calls = append(d.calls, sagaCall{Syscall: syscall, Key: key})
	return d.handler(syscall)
}

func (d *sagaDispatcher) Capabilities() []sys.Capability { return d.caps }

var sagaCaps = []sys.Capability{
	{Name: "clock.now", Compensation: sys.Compensation{Kind: sys.CompensateNone}},
	{Name: "transfer.out", Compensation: sys.Compensation{Kind: sys.CompensateSyscall, Syscall: "transfer.refund"}},
	{Name: "mail.send"}, // undeclared: escalates
	{Name: "transfer.refund", Hidden: true},
}

// executedJournal journals three completed effects: clock.now, transfer.out, mail.send.
func executedJournal(t *testing.T) *memJournal {
	t.Helper()
	journal := newMemJournal()
	tape, err := journaled.NewTape(journal, journaled.Header{ABI: sys.ABIVersion, Program: "sha256:test", Process: "run-1"})
	if err != nil {
		t.Fatalf("new tape: %v", err)
	}
	for _, step := range []struct {
		name   string
		result string
	}{
		{"clock.now", `{"now":"2022-01-01T00:00:00Z"}`},
		{"transfer.out", `{"transfer_id":"t-7","amount":100}`},
		{"mail.send", `{"message_id":"m-9"}`},
	} {
		call := sys.Syscall{Abi: sys.ABIVersion, Name: step.name}
		if _, _, err := tape.Next(call); err != nil {
			t.Fatalf("next %s: %v", step.name, err)
		}
		if _, err := tape.Begin(call); err != nil {
			t.Fatalf("begin %s: %v", step.name, err)
		}
		if err := tape.Commit(sys.Result(json.RawMessage(step.result))); err != nil {
			t.Fatalf("commit %s: %v", step.name, err)
		}
	}
	return journal
}

func TestUnwindCompensatesNewestFirst(t *testing.T) {
	journal := executedJournal(t)
	dispatcher := &sagaDispatcher{
		caps: sagaCaps,
		handler: func(sys.Syscall) (sys.SyscallResult, error) {
			return sys.Result(json.RawMessage(`{"refunded":true}`)), nil
		},
	}

	outcomes, err := Unwind(context.Background(), testPID{id: "run-1"}, journal, 0, dispatcher)
	if err != nil {
		t.Fatalf("unwind: %v", err)
	}
	if len(outcomes) != 3 {
		t.Fatalf("outcomes = %d, want 3: %+v", len(outcomes), outcomes)
	}
	// Newest first: mail.send escalates, transfer.out compensates, clock.now skips.
	if outcomes[0].Original.Name != "mail.send" || outcomes[0].Action != CompensationEscalated {
		t.Fatalf("outcome 0 = %+v, want mail.send escalated", outcomes[0])
	}
	if outcomes[1].Original.Name != "transfer.out" || outcomes[1].Action != CompensationDispatched {
		t.Fatalf("outcome 1 = %+v, want transfer.out compensated", outcomes[1])
	}
	if outcomes[2].Original.Name != "clock.now" || outcomes[2].Action != CompensationSkipped {
		t.Fatalf("outcome 2 = %+v, want clock.now skipped", outcomes[2])
	}

	if len(dispatcher.calls) != 1 || dispatcher.calls[0].Syscall.Name != "transfer.refund" {
		t.Fatalf("dispatched = %+v, want one transfer.refund", dispatcher.calls)
	}
	if dispatcher.calls[0].Key == "" {
		t.Fatal("compensation dispatch carried no idempotency key")
	}
	var args CompensationArgs
	if err := json.Unmarshal(dispatcher.calls[0].Syscall.Args, &args); err != nil {
		t.Fatalf("decode inverse args: %v", err)
	}
	if args.Syscall.Name != "transfer.out" || args.Compensates != 2 {
		t.Fatalf("inverse args = %+v, want the original transfer.out at position 2", args)
	}
	if string(args.Result.Result()) != `{"transfer_id":"t-7","amount":100}` {
		t.Fatalf("inverse args result = %s", args.Result.Result())
	}

	if err := journaled.Verify(journal); err != nil {
		t.Fatalf("verify after unwind: %v", err)
	}
	if journal.Length() != 8 {
		t.Fatalf("journal length = %d, want 8 (3 pairs + 1 compensation pair)", journal.Length())
	}

	// A second unwind is a no-op for the already-compensated effect.
	dispatcher.calls = nil
	outcomes, err = Unwind(context.Background(), testPID{id: "run-1"}, journal, 0, dispatcher)
	if err != nil {
		t.Fatalf("second unwind: %v", err)
	}
	if len(dispatcher.calls) != 0 {
		t.Fatalf("second unwind re-dispatched: %+v", dispatcher.calls)
	}
	for _, outcome := range outcomes {
		if outcome.Action == CompensationDispatched {
			t.Fatalf("second unwind claims a new compensation: %+v", outcome)
		}
	}
}

func TestUnwindYieldsForApprovalAndResumes(t *testing.T) {
	journal := executedJournal(t)
	yieldOnce := true
	dispatcher := &sagaDispatcher{
		caps: sagaCaps,
		handler: func(sys.Syscall) (sys.SyscallResult, error) {
			if yieldOnce {
				yieldOnce = false
				return sys.Yield("awaiting refund approval"), nil
			}
			return sys.Result(json.RawMessage(`{"refunded":true}`)), nil
		},
	}

	_, err := Unwind(context.Background(), testPID{id: "run-1"}, journal, 0, dispatcher)
	if !errors.Is(err, ErrUnwindYielded) {
		t.Fatalf("error = %v, want ErrUnwindYielded", err)
	}

	outcomes, err := Unwind(context.Background(), testPID{id: "run-1"}, journal, 0, dispatcher)
	if err != nil {
		t.Fatalf("resumed unwind: %v", err)
	}
	if len(dispatcher.calls) != 2 {
		t.Fatalf("dispatch count = %d, want 2 (yielded + resumed)", len(dispatcher.calls))
	}
	if dispatcher.calls[0].Key != dispatcher.calls[1].Key {
		t.Fatalf("resumed compensation changed idempotency key: %q -> %q", dispatcher.calls[0].Key, dispatcher.calls[1].Key)
	}
	// The resumed compensation completes, then the remaining effects are handled.
	if outcomes[0].Action != CompensationDispatched || outcomes[0].Compensates != 2 {
		t.Fatalf("outcome 0 = %+v, want the resumed transfer.out compensation", outcomes[0])
	}
	if err := journaled.Verify(journal); err != nil {
		t.Fatalf("verify: %v", err)
	}
}

func TestUnwindEscalatesFailedInverse(t *testing.T) {
	journal := executedJournal(t)
	dispatcher := &sagaDispatcher{
		caps: sagaCaps,
		handler: func(sys.Syscall) (sys.SyscallResult, error) {
			return sys.FailCode(sys.ErrnoDenied, "refund window closed"), nil
		},
	}

	outcomes, err := Unwind(context.Background(), testPID{id: "run-1"}, journal, 0, dispatcher)
	if err != nil {
		t.Fatalf("unwind: %v", err)
	}
	var escalated *CompensationOutcome
	for i := range outcomes {
		if outcomes[i].Compensates == 2 {
			escalated = &outcomes[i]
		}
	}
	if escalated == nil || escalated.Action != CompensationEscalated {
		t.Fatalf("outcomes = %+v, want the failed inverse escalated", outcomes)
	}
	if escalated.Result.Errno() != sys.ErrnoDenied {
		t.Fatalf("escalated result = %#v", escalated.Result)
	}
	if err := journaled.Verify(journal); err != nil {
		t.Fatalf("verify: %v", err)
	}
}

func TestUnwindRespectsScopeStart(t *testing.T) {
	journal := executedJournal(t)
	dispatcher := &sagaDispatcher{
		caps: sagaCaps,
		handler: func(sys.Syscall) (sys.SyscallResult, error) {
			return sys.Result(nil), nil
		},
	}

	// Scope starts at position 4 (mail.send's intent): transfer.out at 2 is outside.
	outcomes, err := Unwind(context.Background(), testPID{id: "run-1"}, journal, 4, dispatcher)
	if err != nil {
		t.Fatalf("unwind: %v", err)
	}
	if len(outcomes) != 1 || outcomes[0].Original.Name != "mail.send" {
		t.Fatalf("outcomes = %+v, want only mail.send", outcomes)
	}
	if len(dispatcher.calls) != 0 {
		t.Fatalf("out-of-scope effect was compensated: %+v", dispatcher.calls)
	}
}

func TestResumeRefusedAfterUnwind(t *testing.T) {
	journal := executedJournal(t)
	dispatcher := &sagaDispatcher{
		caps: sagaCaps,
		handler: func(sys.Syscall) (sys.SyscallResult, error) {
			return sys.Result(nil), nil
		},
	}
	if _, err := Unwind(context.Background(), testPID{id: "run-1"}, journal, 0, dispatcher); err != nil {
		t.Fatalf("unwind: %v", err)
	}

	tape, err := journaled.NewTape(journal, journaled.Header{ABI: sys.ABIVersion, Program: "sha256:test", Process: "run-1"})
	if err != nil {
		t.Fatalf("new tape: %v", err)
	}
	// Replay the three recorded pairs, then the guest asks for more.
	for _, name := range []string{"clock.now", "transfer.out", "mail.send"} {
		if _, replayed, err := tape.Next(sys.Syscall{Abi: sys.ABIVersion, Name: name}); err != nil || !replayed {
			t.Fatalf("replay %s: replayed=%v err=%v", name, replayed, err)
		}
	}
	_, _, err = tape.Next(sys.Syscall{Abi: sys.ABIVersion, Name: "another.call"})
	var unwound journaled.ProcessUnwoundError
	if !errors.As(err, &unwound) {
		t.Fatalf("error = %v, want ProcessUnwoundError", err)
	}
}
