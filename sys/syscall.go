package sys

import (
	"encoding/json"
	"sort"
)

// ABIVersion is the syscall wire version this kernel speaks. Guests declare
// it on every Syscall; the host rejects mismatches with ErrnoBadABI.
const ABIVersion = 2

// Reserved syscall names. Savepoint brackets are journaled as side-effect-free
// markers by the host: on a failed-run resume, the journal is forked just past
// the outermost unclosed Begin so the whole declared unit re-executes.
// Brackets follow stack semantics: a Commit closes the most recent open Begin.
const (
	SyscallBegin  = "sys.begin"
	SyscallCommit = "sys.commit"
	// SyscallSpawn creates a child process (sync-first: the parent's quantum
	// runs the child; a yielding child yields the parent transitively). The
	// kernel's Spawner decorator serves it.
	SyscallSpawn = "sys.spawn"
)

// Syscall is the guest-to-host request crossing the syscall boundary.
type Syscall struct {
	Abi  int             `json:"abi"`
	Name string          `json:"name"`
	Args json.RawMessage `json:"args,omitempty"`
}

func (sc Syscall) Copy() Syscall {
	sc.Args = append(json.RawMessage(nil), sc.Args...)
	return sc
}

// SyscallStatus identifies a handler or replay result.
type SyscallStatus string

const (
	StatusResult SyscallStatus = "result"
	StatusYield  SyscallStatus = "yield"
	StatusFailed SyscallStatus = "failed"
)

// Errno is the machine-readable failure class carried alongside the human
// message, so guests branch on a closed set instead of parsing prose.
type Errno string

const (
	ErrnoDenied      Errno = "denied"       // authorization refused the operation
	ErrnoExpired     Errno = "expired"      // a task or grant passed its deadline
	ErrnoNotFound    Errno = "not_found"    // no handler/tool/resource by that name
	ErrnoInvalidArgs Errno = "invalid_args" // request failed validation/decoding
	ErrnoTransient   Errno = "transient"    // infrastructure failure; retry may succeed
	ErrnoInternal    Errno = "internal"     // unclassified failure
	ErrnoBadABI      Errno = "bad_abi"      // syscall ABI version mismatch
)

// SyscallResult is the ADT returned to guest syscalls.
type SyscallResult struct {
	status  SyscallStatus
	errno   Errno
	result  json.RawMessage
	message string
	labels  []string
}

func Result(result json.RawMessage) SyscallResult {
	return SyscallResult{status: StatusResult, result: append(json.RawMessage(nil), result...)}
}

func Yield(message string) SyscallResult {
	return SyscallResult{status: StatusYield, message: message}
}

// Fail returns an unclassified (ErrnoInternal) failure. Prefer FailCode.
func Fail(message string) SyscallResult {
	return FailCode(ErrnoInternal, message)
}

// FailCode returns a failure classified by errno.
func FailCode(errno Errno, message string) SyscallResult {
	if message == "" {
		message = "command failed"
	}
	if errno == "" {
		errno = ErrnoInternal
	}
	return SyscallResult{status: StatusFailed, errno: errno, message: message}
}

func (r SyscallResult) Status() SyscallStatus {
	return r.status
}

// Errno returns the failure class; empty unless Status is StatusFailed.
func (r SyscallResult) Errno() Errno {
	return r.errno
}

func (r SyscallResult) Result() json.RawMessage {
	return append(json.RawMessage(nil), r.result...)
}

func (r SyscallResult) Message() string {
	return r.message
}

// Labels returns the provenance labels stamped on this result — the source
// classes its data derives from. Sorted and deduplicated.
func (r SyscallResult) Labels() []string {
	return append([]string(nil), r.labels...)
}

// WithLabels returns a copy of the result carrying the union of its labels
// and the given ones, sorted and deduplicated (so journal digests are
// deterministic).
func (r SyscallResult) WithLabels(labels ...string) SyscallResult {
	if len(labels) == 0 {
		return r
	}
	merged := make(map[string]struct{}, len(r.labels)+len(labels))
	for _, label := range r.labels {
		merged[label] = struct{}{}
	}
	for _, label := range labels {
		if label != "" {
			merged[label] = struct{}{}
		}
	}
	r.labels = make([]string, 0, len(merged))
	for label := range merged {
		r.labels = append(r.labels, label)
	}
	sort.Strings(r.labels)
	return r
}

func (r SyscallResult) Copy() SyscallResult {
	r.result = append(json.RawMessage(nil), r.result...)
	r.labels = append([]string(nil), r.labels...)
	return r
}

// syscallResultJSON is the durable/wire rendering of a SyscallResult — the
// same field set the host returns to guests.
type syscallResultJSON struct {
	Status  SyscallStatus   `json:"status"`
	Code    Errno           `json:"code,omitempty"`
	Result  json.RawMessage `json:"result,omitempty"`
	Message string          `json:"message,omitempty"`
	Labels  []string        `json:"labels,omitempty"`
}

func (r SyscallResult) MarshalJSON() ([]byte, error) {
	return json.Marshal(syscallResultJSON{
		Status:  r.status,
		Code:    r.errno,
		Result:  r.result,
		Message: r.message,
		Labels:  r.labels,
	})
}

func (r *SyscallResult) UnmarshalJSON(data []byte) error {
	var decoded syscallResultJSON
	if err := json.Unmarshal(data, &decoded); err != nil {
		return err
	}
	*r = SyscallResult{
		status:  decoded.Status,
		errno:   decoded.Code,
		result:  decoded.Result,
		message: decoded.Message,
		labels:  decoded.Labels,
	}
	return nil
}
