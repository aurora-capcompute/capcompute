package journaled_test

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/aurora-capcompute/capcompute/sys"
	"github.com/aurora-capcompute/capcompute/sys/replay/tape/journaled"
)

// A host-authored abort after a completed pair reads back as a normal terminal
// pair — same shape a guest's own sys.abort leaves — and the chain verifies,
// compensation section included.
func TestAbortClosesCleanTail(t *testing.T) {
	journal := &fakeJournal{}
	tape := newTestTape(t, journal)
	record(t, tape, sys.Syscall{Abi: sys.ABIVersion, Name: "billing.charge"}, sys.Result([]byte(`{"id":"c1"}`)))

	args := json.RawMessage(`{"reason":"guest failed","cause":"failure"}`)
	if err := journaled.Abort(journal, args); err != nil {
		t.Fatalf("abort: %v", err)
	}
	if journal.Length() != 4 {
		t.Fatalf("length = %d, want the charge pair plus the abort pair", journal.Length())
	}
	intent, _ := journal.Load(2)
	if intent.Kind != journaled.KindIntent || intent.Syscall == nil ||
		intent.Syscall.Name != sys.SyscallAbort || string(intent.Syscall.Args) != string(args) {
		t.Fatalf("abort intent = %+v", intent)
	}
	completion, _ := journal.Load(3)
	if completion.Kind != journaled.KindCompletion || completion.Result == nil ||
		completion.Result.Status() != sys.StatusResult {
		t.Fatalf("abort completion = %+v", completion)
	}
	if err := journaled.Verify(journal); err != nil {
		t.Fatalf("verify after abort: %v", err)
	}

	// The compensation section follows the abort like any rollback's.
	compensator, err := journaled.NewCompensator(journal)
	if err != nil {
		t.Fatalf("new compensator: %v", err)
	}
	if _, err := compensator.Begin(sys.Syscall{Abi: sys.ABIVersion, Name: "billing.refund"}, 0); err != nil {
		t.Fatalf("compensation begin: %v", err)
	}
	if err := compensator.Commit(sys.Result([]byte(`{}`))); err != nil {
		t.Fatalf("compensation commit: %v", err)
	}
	if err := journaled.Verify(journal); err != nil {
		t.Fatalf("verify after compensation: %v", err)
	}
}

// The one legal append over an open intent: the dispatch never completed (its
// intent stays open, indeterminate), the host aborts the section, compensation
// follows — and Verify accepts the whole story.
func TestAbortClosesOverOpenIntent(t *testing.T) {
	journal := &fakeJournal{}
	tape := newTestTape(t, journal)
	record(t, tape, sys.Syscall{Abi: sys.ABIVersion, Name: "billing.charge"}, sys.Result([]byte(`{"id":"c1"}`)))
	if _, err := tape.Begin(sys.Syscall{Abi: sys.ABIVersion, Name: "flaky.call"}); err != nil {
		t.Fatalf("begin open intent: %v", err)
	}

	if err := journaled.Abort(journal, json.RawMessage(`{"reason":"driver error","cause":"failure"}`)); err != nil {
		t.Fatalf("abort over open intent: %v", err)
	}
	if err := journaled.Verify(journal); err != nil {
		t.Fatalf("verify: %v", err)
	}

	compensator, err := journaled.NewCompensator(journal)
	if err != nil {
		t.Fatalf("new compensator: %v", err)
	}
	if _, err := compensator.Begin(sys.Syscall{Abi: sys.ABIVersion, Name: "billing.refund"}, 0); err != nil {
		t.Fatalf("compensation begin: %v", err)
	}
	if err := compensator.Commit(sys.Result([]byte(`{}`))); err != nil {
		t.Fatalf("compensation commit: %v", err)
	}
	if err := journaled.Verify(journal); err != nil {
		t.Fatalf("verify with open intent + abort + compensation: %v", err)
	}

	// The open intent is history, not an effect: the compensator never offers
	// an indeterminate call for mechanical undo.
	effects, resume, err := compensator.Effects(0)
	if err != nil || resume != nil {
		t.Fatalf("effects: resume=%v err=%v", resume, err)
	}
	for _, effect := range effects {
		if effect.Syscall.Name == "flaky.call" {
			t.Fatal("an open (indeterminate) intent surfaced as a compensable effect")
		}
	}
}

// A journal that never executed anything has no header and nothing to abort.
func TestAbortRequiresHeader(t *testing.T) {
	err := journaled.Abort(&fakeJournal{}, nil)
	if err == nil || !strings.Contains(err.Error(), "no header") {
		t.Fatalf("abort on headerless journal = %v, want a header error", err)
	}
}
