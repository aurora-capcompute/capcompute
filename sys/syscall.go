package sys

import (
	"bytes"
	"encoding/json"
	"maps"
	"slices"
)

// ABIVersion is the syscall wire version this processor speaks. Guests declare
// it on every Syscall, and the host refuses a mismatch with ErrnoBadABI. The
// wire encoding is JSON — a Syscall in, a SyscallResult out — so the JSON tags
// on these types are the wire, the journal, and the audit rendering at once.
const ABIVersion = 4

// Reserved syscall names: the runtime serves these itself, for every program.
const (
	// SyscallBegin and SyscallCommit bracket a section that is safe to run
	// again. They have no effect. Brackets nest: a commit closes the most recent
	// open begin. When a run fails inside a section, the next run forks the
	// journal just past the outermost open begin, so the whole section runs
	// again.
	SyscallBegin  = "sys.begin"
	SyscallCommit = "sys.commit"
	// SyscallInput fetches the process's input and SyscallOutput records its
	// answer. SyscallLog reports progress.
	SyscallInput  = "sys.input"
	SyscallOutput = "sys.output"
	SyscallLog    = "sys.log"
	// SyscallAbort abandons the open section and runs it again.
	SyscallAbort = "sys.abort"
	// SyscallTimer parks the process until a duration passes.
	SyscallTimer = "sys.timer"
	// SyscallNow and SyscallRandom read the real clock and real entropy. The
	// journal records the values, so a replay sees the same ones.
	SyscallNow    = "sys.now"
	SyscallRandom = "sys.random"
)

// Syscall is the guest-to-host request crossing the syscall boundary.
type Syscall struct {
	Abi  int             `json:"abi"`
	Name string          `json:"name"`
	Args json.RawMessage `json:"args,omitempty"`
}

// Copy returns the syscall with its own copy of the args.
func (sc Syscall) Copy() Syscall {
	sc.Args = append(json.RawMessage(nil), sc.Args...)
	return sc
}

// SyscallStatus is the kind of a SyscallResult.
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

// SyscallResult is the outcome of one syscall: a result, a yield (the call
// waits on something outside the process), or a failure with an Errno. It also
// carries provenance labels, which the host keeps and never shows the guest.
// It is a value: every accessor returns a copy.
type SyscallResult struct {
	status  SyscallStatus
	errno   Errno
	result  json.RawMessage
	message string
	labels  []string
}

// Result returns a successful outcome carrying the result JSON.
func Result(result json.RawMessage) SyscallResult {
	return SyscallResult{status: StatusResult, result: append(json.RawMessage(nil), result...)}
}

// Yield returns an outcome that parks the process. message says what it waits
// on.
func Yield(message string) SyscallResult {
	return SyscallResult{status: StatusYield, message: message}
}

// Fail returns an unclassified (ErrnoInternal) failure. Prefer FailCode.
func Fail(message string) SyscallResult {
	return FailCode(ErrnoInternal, message)
}

// FailCode returns a failure classified by errno.
func FailCode(errno Errno, message string) SyscallResult {
	return SyscallResult{status: StatusFailed, errno: errno, message: message}
}

func (r SyscallResult) Status() SyscallStatus {
	return r.status
}

// Errno returns the failure class; empty unless Status is StatusFailed.
func (r SyscallResult) Errno() Errno {
	return r.errno
}

// Result returns the result JSON. It is empty unless Status is StatusResult.
func (r SyscallResult) Result() json.RawMessage {
	return append(json.RawMessage(nil), r.result...)
}

// Message returns the human text of a failure or a yield.
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
	r.labels = slices.Sorted(maps.Keys(merged))
	return r
}

// Copy returns the result with its own copies of the result and the labels.
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

// MarshalJSON renders the durable/wire form without HTML escaping: the
// guest's result bytes must survive storage verbatim, or a restored journal
// would carry \u003c-escaped bytes the guest never produced.
func (r SyscallResult) MarshalJSON() ([]byte, error) {
	var buf bytes.Buffer
	encoder := json.NewEncoder(&buf)
	encoder.SetEscapeHTML(false)
	if err := encoder.Encode(syscallResultJSON{
		Status:  r.status,
		Code:    r.errno,
		Result:  r.result,
		Message: r.message,
		Labels:  r.labels,
	}); err != nil {
		return nil, err
	}
	return bytes.TrimRight(buf.Bytes(), "\n"), nil
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
