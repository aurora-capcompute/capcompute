package journaled

import (
	"encoding/json"
	"fmt"

	"github.com/aurora-capcompute/capcompute/sys"
)

// Abort appends a host-authored terminal abort: the sys.abort intent that
// closes the journal's execution section, and its success completion. A host
// aborts a section on the guest's behalf when the guest cannot — it failed, or
// was stopped — so the rollback trigger becomes a journal fact that every
// crash-resume path re-derives, exactly like a guest-called sys.abort (which
// arrives through the tape and encodes identically).
//
// This is the one append permitted after an open intent at the tail: the
// failing dispatch never completed, and its intent stays open — indeterminate,
// legal history (see Verify). No further execution may be appended; only the
// compensation section follows.
func Abort(journal Journal, args json.RawMessage) error {
	header, ok, err := journal.Header()
	if err != nil {
		return err
	}
	if !ok {
		return fmt.Errorf("abort: journal has no header — nothing executed, nothing to abort")
	}
	call := sys.Syscall{Abi: sys.ABIVersion, Name: sys.SyscallAbort, Args: append(json.RawMessage(nil), args...)}
	if _, err := appendChained(journal, header, Record{Kind: KindIntent, Syscall: &call}); err != nil {
		return err
	}
	result := sys.Result(json.RawMessage(`{"ok":true}`))
	_, err = appendChained(journal, header, Record{Kind: KindCompletion, Result: &result})
	return err
}
