package journaled_test

import (
	"capcompute/dispatcher"
	"capcompute/dispatcher/replay/tape/journaled"
	"testing"
)

// fakeJournal is an in-memory journaled.Journal. The tape needs only the Journal
// contract, so the package's own tests supply it directly rather than depending
// on a concrete store from a consumer module.
type fakeJournal struct {
	records []journaled.Record
}

func (j *fakeJournal) Load(idx int) (journaled.Record, error) {
	return j.records[idx], nil
}

func (j *fakeJournal) Store(_ int, call dispatcher.Call, outcome dispatcher.Outcome) error {
	j.records = append(j.records, journaled.Record{Call: call, Outcome: outcome})
	return nil
}

func (j *fakeJournal) Length() int { return len(j.records) }

func TestRecordConsumesNewRecord(t *testing.T) {
	tape := journaled.NewTape(&fakeJournal{})

	if err := tape.Record(dispatcher.Call{Name: "step.one"}, dispatcher.Result(nil)); err != nil {
		t.Fatalf("record: %v", err)
	}
	if remaining := tape.Remaining(); remaining != 0 {
		t.Fatalf("remaining = %d, want 0", remaining)
	}
}

func TestResetReplaysRecordedResults(t *testing.T) {
	tape := journaled.NewTape(&fakeJournal{})
	call := dispatcher.Call{Name: "step.one"}

	if err := tape.Record(call, dispatcher.Result([]byte(`{"ok":true}`))); err != nil {
		t.Fatalf("record: %v", err)
	}

	tape.Reset()
	outcome, ok, err := tape.Next(call)
	if err != nil {
		t.Fatalf("next: %v", err)
	}
	if !ok {
		t.Fatal("record was not replayed")
	}
	if string(outcome.Result()) != `{"ok":true}` {
		t.Fatalf("result = %s", outcome.Result())
	}
	if remaining := tape.Remaining(); remaining != 0 {
		t.Fatalf("remaining = %d, want 0", remaining)
	}
}

func TestResetReplaysRecordedFailures(t *testing.T) {
	tape := journaled.NewTape(&fakeJournal{})
	call := dispatcher.Call{Name: "step.one"}

	if err := tape.Record(call, dispatcher.Failed("permission denied")); err != nil {
		t.Fatalf("record: %v", err)
	}

	tape.Reset()
	outcome, ok, err := tape.Next(call)
	if err != nil {
		t.Fatalf("next: %v", err)
	}
	if !ok {
		t.Fatal("failure was not replayed")
	}
	if outcome.Kind() != dispatcher.OutcomeFailed || outcome.Message() != "permission denied" {
		t.Fatalf("outcome = %#v", outcome)
	}
}

func TestYieldIsNotRecorded(t *testing.T) {
	journal := &fakeJournal{}
	tape := journaled.NewTape(journal)
	if err := tape.Record(dispatcher.Call{Name: "step.one"}, dispatcher.Yield("waiting")); err != nil {
		t.Fatalf("record yield: %v", err)
	}
	if journal.Length() != 0 {
		t.Fatalf("journal length = %d, want 0", journal.Length())
	}
}
