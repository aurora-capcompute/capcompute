package journaled_test

import (
	"aurora-stores/memory"
	"capcompute/dispatcher"
	"capcompute/dispatcher/replay/tape/journaled"
	"testing"
)

func TestRecordConsumesNewRecord(t *testing.T) {
	tape := journaled.NewTape(memory.NewJournal())

	if err := tape.Record(dispatcher.Call{Name: "step.one"}, dispatcher.Result(nil)); err != nil {
		t.Fatalf("record: %v", err)
	}
	if remaining := tape.Remaining(); remaining != 0 {
		t.Fatalf("remaining = %d, want 0", remaining)
	}
}

func TestResetReplaysRecordedResults(t *testing.T) {
	tape := journaled.NewTape(memory.NewJournal())
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
	tape := journaled.NewTape(memory.NewJournal())
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
	journal := memory.NewJournal()
	tape := journaled.NewTape(journal)
	if err := tape.Record(dispatcher.Call{Name: "step.one"}, dispatcher.Yield("waiting")); err != nil {
		t.Fatalf("record yield: %v", err)
	}
	if journal.Length() != 0 {
		t.Fatalf("journal length = %d, want 0", journal.Length())
	}
}
