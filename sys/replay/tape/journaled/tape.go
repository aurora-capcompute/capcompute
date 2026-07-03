// Package journaled is a replay Tape backed by an append-only Journal of
// intent/completion records: an intent is appended before a syscall executes
// (journal-before-execute), a completion after its outcome is known and
// before the guest observes it (journal-before-observe). It serves recorded
// pairs in order, raises ReplayDivergedError when a re-run guest issues a
// syscall that does not match the recorded sequence, and surfaces an intent
// without completion as replay.OpenIntentError. It owns the on-tape record
// format — a fixed envelope (position, kind, prev_hash) plus an opaque
// syscall/result payload, hash-chained for tamper evidence; the Journal
// itself — where records durably live — is supplied by the caller.
package journaled

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"

	"github.com/aurora-capcompute/capcompute/sys"
	"github.com/aurora-capcompute/capcompute/sys/replay"
)

// Kind classifies a journal record within the fixed envelope.
type Kind string

const (
	// KindIntent records that a syscall is about to execute.
	KindIntent Kind = "intent"
	// KindCompletion records the outcome of the immediately preceding intent.
	KindCompletion Kind = "completion"
	// KindCompensationIntent records that an inverse syscall is about to
	// undo the completed effect at the position its Compensates field names.
	// The first compensation record ends the journal's execution section:
	// an unwound run is terminal.
	KindCompensationIntent Kind = "compensation_intent"
	// KindCompensationCompletion records the outcome of the immediately
	// preceding compensation intent.
	KindCompensationCompletion Kind = "compensation_completion"
)

// Header identifies what wrote a journal: the syscall ABI version, the
// program (e.g. an artifact digest), and the run (PID) the journal belongs
// to. Replay against a different writer is refused up front — the
// versioned-replay law (see docs/ARCHITECTURE.md, "Coherence under growth")
// — instead of failing later as a confusing divergence. Run also scopes the
// intent identity, so idempotency keys are unique across runs.
type Header struct {
	ABI     int    `json:"abi"`
	Program string `json:"program"`
	Run     string `json:"run,omitempty"`
}

// Record is one journal entry: a fixed envelope — the store's index keys —
// plus an opaque payload. A datum lives in the envelope or in the payload,
// never both. Scope columns above the run (tenant, thread, revision) belong
// to the store that owns the Journal, keyed by Header.Run.
type Record struct {
	// Envelope.
	Position int    `json:"position"`
	Kind     Kind   `json:"kind"`
	PrevHash string `json:"prev_hash"`
	// Compensates names the intent position a compensation record undoes.
	Compensates *int `json:"compensates,omitempty"`

	// Payload: exactly one of the two, by Kind.
	Syscall *sys.Syscall       `json:"syscall,omitempty"`
	Result  *sys.SyscallResult `json:"result,omitempty"`
}

// Journal stores durable records for a tape, plus the header identifying
// their writer. Append must persist the record at index Length().
type Journal interface {
	Header() (header Header, ok bool, err error)
	SetHeader(Header) error
	Load(idx int) (Record, error)
	Append(Record) error
	Length() int
}

// ReplayDivergedError means the guest requested a different syscall than history recorded.
type ReplayDivergedError struct {
	Index int
	Want  sys.Syscall
	Got   sys.Syscall
}

func (e ReplayDivergedError) Error() string {
	return fmt.Sprintf("replay diverged at syscall %d: want %q got %q", e.Index, e.Want.Name, e.Got.Name)
}

// ReplayIncompatibleError means the journal was written by a different program
// or ABI than the one attempting to replay it.
type ReplayIncompatibleError struct {
	Recorded Header
	Current  Header
}

func (e ReplayIncompatibleError) Error() string {
	return fmt.Sprintf("journal written by program %q (abi %d); cannot replay as program %q (abi %d)",
		e.Recorded.Program, e.Recorded.ABI, e.Current.Program, e.Current.ABI)
}

// CorruptJournalError means the journal's record structure is invalid — not a
// divergence (guest behavior) but damage or a buggy store.
type CorruptJournalError struct {
	Position int
	Reason   string
}

func (e CorruptJournalError) Error() string {
	return fmt.Sprintf("journal corrupt at position %d: %s", e.Position, e.Reason)
}

// ChainBrokenError means a record's prev_hash does not match the digest of
// its predecessor: the journal was altered after the fact.
type ChainBrokenError struct {
	Position int
}

func (e ChainBrokenError) Error() string {
	return fmt.Sprintf("journal hash chain broken at position %d", e.Position)
}

// RunUnwoundError means execution replay reached the journal's compensation
// section: this run was aborted and its effects compensated; it cannot be
// resumed.
type RunUnwoundError struct {
	Position int
}

func (e RunUnwoundError) Error() string {
	return fmt.Sprintf("run was unwound: compensation section starts at position %d", e.Position)
}

// Tape is a journal-backed replay tape.
type Tape struct {
	journal Journal
	header  Header
	cursor  int
	pending bool // an intent is open and owned by the in-flight dispatch
}

// NewTape creates a journal-backed replay tape whose cursor starts at the
// beginning. The header identifies the program about to run: a fresh journal
// is stamped with it; a journal written by a different program, ABI, or run
// is refused with ReplayIncompatibleError.
func NewTape(journal Journal, header Header) (*Tape, error) {
	recorded, ok, err := journal.Header()
	if err != nil {
		return nil, err
	}
	if !ok {
		if err := journal.SetHeader(header); err != nil {
			return nil, err
		}
		return &Tape{journal: journal, header: header}, nil
	}
	if recorded != header {
		return nil, ReplayIncompatibleError{Recorded: recorded, Current: header}
	}
	return &Tape{journal: journal, header: header}, nil
}

// Next returns a recorded outcome for syscall, ok=false when syscall is new,
// or replay.OpenIntentError when the journal ends in this syscall's intent
// with no completion.
func (t *Tape) Next(syscall sys.Syscall) (sys.SyscallResult, bool, error) {
	if t == nil || t.cursor >= t.journal.Length() {
		return sys.SyscallResult{}, false, nil
	}

	intent, err := t.journal.Load(t.cursor)
	if err != nil {
		return sys.SyscallResult{}, false, err
	}
	if intent.Kind == KindCompensationIntent || intent.Kind == KindCompensationCompletion {
		return sys.SyscallResult{}, false, RunUnwoundError{Position: t.cursor}
	}
	if intent.Kind != KindIntent || intent.Syscall == nil {
		return sys.SyscallResult{}, false, CorruptJournalError{Position: t.cursor, Reason: "expected an intent record"}
	}
	if !sameSyscall(*intent.Syscall, syscall) {
		return sys.SyscallResult{}, false, ReplayDivergedError{
			Index: t.cursor,
			Want:  *intent.Syscall,
			Got:   syscall,
		}
	}

	if t.cursor+1 >= t.journal.Length() {
		// Open intent at the tail: dispatched once, outcome unknown. Hand the
		// original intent identity back so a retry carries the same key.
		key, err := t.intentKey(intent.Position, syscall)
		if err != nil {
			return sys.SyscallResult{}, false, err
		}
		t.cursor++
		t.pending = true
		return sys.SyscallResult{}, false, replay.OpenIntentError{
			Position: intent.Position,
			Key:      key,
			Syscall:  syscall,
		}
	}

	completion, err := t.journal.Load(t.cursor + 1)
	if err != nil {
		return sys.SyscallResult{}, false, err
	}
	if completion.Kind == KindCompensationIntent || completion.Kind == KindCompensationCompletion {
		return sys.SyscallResult{}, false, RunUnwoundError{Position: t.cursor + 1}
	}
	if completion.Kind != KindCompletion || completion.Result == nil {
		return sys.SyscallResult{}, false, CorruptJournalError{Position: t.cursor + 1, Reason: "expected a completion record"}
	}
	t.cursor += 2
	return *completion.Result, true, nil
}

// Begin appends the intent record for a fresh syscall — before it executes —
// and returns its idempotency key.
func (t *Tape) Begin(syscall sys.Syscall) (string, error) {
	if t == nil {
		return "", fmt.Errorf("begin on nil tape")
	}
	if t.pending {
		return "", fmt.Errorf("begin: an intent is already open at position %d", t.journal.Length()-1)
	}
	if t.cursor != t.journal.Length() {
		return "", fmt.Errorf("begin: cursor %d has not consumed the journal (length %d)", t.cursor, t.journal.Length())
	}

	position := t.journal.Length()
	prev, err := t.prevHash(position)
	if err != nil {
		return "", err
	}
	recorded := syscall.Copy()
	if err := t.journal.Append(Record{
		Position: position,
		Kind:     KindIntent,
		PrevHash: prev,
		Syscall:  &recorded,
	}); err != nil {
		return "", err
	}
	t.cursor++
	t.pending = true
	return t.intentKey(position, syscall)
}

// Commit appends the completion record for the open intent — after the
// outcome is known and before the guest observes it.
func (t *Tape) Commit(result sys.SyscallResult) error {
	if t == nil || !t.pending {
		return fmt.Errorf("commit: no open intent")
	}
	if result.Status() != sys.StatusResult && result.Status() != sys.StatusFailed {
		return fmt.Errorf("cannot commit invalid result %q", result.Status())
	}

	position := t.journal.Length()
	prev, err := t.prevHash(position)
	if err != nil {
		return err
	}
	recorded := result.Copy()
	if err := t.journal.Append(Record{
		Position: position,
		Kind:     KindCompletion,
		PrevHash: prev,
		Result:   &recorded,
	}); err != nil {
		return err
	}
	t.cursor++
	t.pending = false
	return nil
}

func (t *Tape) Reset() {
	if t == nil {
		return
	}
	t.cursor = 0
	t.pending = false
}

func (t *Tape) Remaining() int {
	if t == nil {
		return 0
	}
	return t.journal.Length() - t.cursor
}

// Verify walks the whole journal and checks structure (alternating
// intent/completion, positions in order, at most one open intent at the tail)
// and the hash chain. It returns ChainBrokenError or CorruptJournalError at
// the first damage found.
func Verify(journal Journal) error {
	header, ok, err := journal.Header()
	if err != nil {
		return err
	}
	if !ok {
		if journal.Length() > 0 {
			return CorruptJournalError{Position: 0, Reason: "records present without a header"}
		}
		return nil
	}

	prev, err := digest(header)
	if err != nil {
		return err
	}
	// Structure: an execution section of intent/completion pairs (at most one
	// trailing open intent), optionally followed by a terminal compensation
	// section of compensation pairs (at most one trailing open compensation
	// intent). The abort that triggered unwinding may leave the execution
	// section's last intent open forever — that is legal history.
	const (
		wantIntent = iota
		wantCompletion
		wantCompensationIntent
		wantCompensationCompletion
	)
	expect := wantIntent
	for position := 0; position < journal.Length(); position++ {
		record, err := journal.Load(position)
		if err != nil {
			return err
		}
		if record.Position != position {
			return CorruptJournalError{Position: position, Reason: fmt.Sprintf("record carries position %d", record.Position)}
		}
		if (expect == wantIntent || expect == wantCompletion) && record.Kind == KindCompensationIntent {
			expect = wantCompensationIntent // the execution section is over
		}
		switch expect {
		case wantIntent:
			if record.Kind != KindIntent || record.Syscall == nil {
				return CorruptJournalError{Position: position, Reason: "expected an intent record"}
			}
			expect = wantCompletion
		case wantCompletion:
			if record.Kind != KindCompletion || record.Result == nil {
				return CorruptJournalError{Position: position, Reason: "expected a completion record"}
			}
			expect = wantIntent
		case wantCompensationIntent:
			if record.Kind != KindCompensationIntent || record.Syscall == nil || record.Compensates == nil {
				return CorruptJournalError{Position: position, Reason: "expected a compensation intent record"}
			}
			expect = wantCompensationCompletion
		case wantCompensationCompletion:
			if record.Kind != KindCompensationCompletion || record.Result == nil {
				return CorruptJournalError{Position: position, Reason: "expected a compensation completion record"}
			}
			expect = wantCompensationIntent
		}
		if record.PrevHash != prev {
			return ChainBrokenError{Position: position}
		}
		prev, err = digest(record)
		if err != nil {
			return err
		}
	}
	return nil
}

// intentKey is the intent identity — (run, position, call-hash) — used as the
// idempotency key handed to drivers. It is deterministic, so a crash-retry of
// the same intent recomputes the same key.
func (t *Tape) intentKey(position int, syscall sys.Syscall) (string, error) {
	return intentKey(t.header, position, syscall)
}

func (t *Tape) prevHash(position int) (string, error) {
	return prevHash(t.journal, t.header, position)
}

func intentKey(header Header, position int, syscall sys.Syscall) (string, error) {
	return digest(struct {
		Header   Header      `json:"header"`
		Position int         `json:"position"`
		Syscall  sys.Syscall `json:"syscall"`
	}{header, position, syscall})
}

func prevHash(journal Journal, header Header, position int) (string, error) {
	if position == 0 {
		return digest(header)
	}
	prev, err := journal.Load(position - 1)
	if err != nil {
		return "", err
	}
	return digest(prev)
}

func digest(v any) (string, error) {
	data, err := json.Marshal(v)
	if err != nil {
		return "", err
	}
	sum := sha256.Sum256(data)
	return hex.EncodeToString(sum[:]), nil
}

func sameSyscall(left sys.Syscall, right sys.Syscall) bool {
	return left.Name == right.Name && bytes.Equal(left.Args, right.Args)
}
