package capcompute

import (
	"errors"
	"testing"
)

type testPID struct {
	id    string
	value string
}

func (k testPID) PID() string {
	return k.id
}

func TestResumeStatusReadsYieldedOutput(t *testing.T) {
	got, err := resumeStatus([]byte(`{"status":"yielded"}`))
	if err != nil {
		t.Fatalf("resume status: %v", err)
	}
	if got != ResumeYielded {
		t.Fatalf("status = %s, want %s", got, ResumeYielded)
	}
}

func TestResumeStatusReadsCompletedOutput(t *testing.T) {
	got, err := resumeStatus([]byte(`{"status":"completed"}`))
	if err != nil {
		t.Fatalf("resume status: %v", err)
	}
	if got != ResumeCompleted {
		t.Fatalf("status = %s, want %s", got, ResumeCompleted)
	}
}

func TestResumeStatusRejectsInvalidOutput(t *testing.T) {
	for _, output := range [][]byte{
		[]byte(`{"answer":"done"}`),
		[]byte(`{"status":"unknown"}`),
		[]byte(`not json`),
	} {
		if _, err := resumeStatus(output); !errors.Is(err, ErrInvalidGuestOutput) {
			t.Fatalf("error = %v for %s, want ErrInvalidGuestOutput", err, output)
		}
	}
}
