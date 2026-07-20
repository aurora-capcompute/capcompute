//go:build wasip1

// Command adversary_guest is a deliberately hostile Extism guest used by
// capcompute's security tests. Like testdata/integration_guest, it is built
// with the standard Go toolchain (GOOS=wasip1 GOARCH=wasm go build
// -buildmode=c-shared), so the adversarial proofs run in any environment that
// already has `go` — no extra toolchain, no silent skip.
//
// Every mode attempts to break one of the two guarantees the processor makes to
// the host that embeds it:
//
//   - host isolation (law #1): the guest must not be able to touch the
//     host filesystem, environment, args, or network. These modes report
//     whether they "escaped"; the test asserts they never do.
//   - the ABI trust boundary: the guest must not be able to forge the ABI
//     version or slip non-wire bytes past the host function. These modes
//     report the errno the host handed back; the test asserts it is the
//     refusal it expects.
//
// The guest cannot tell the difference between "the host let me do it" and
// "the host stopped me" except by what it observes, so it reports its raw
// observations as JSON and lets the Go test be the judge.
package main

import (
	crand "crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"time"

	"github.com/extism/go-pdk"

	"github.com/aurora-capcompute/capcompute/sys"
)

//go:wasmimport extism:host/compute syscall
func hostSyscall(uint64) uint64

type input struct {
	Mode string `json:"mode"`
}

// report is the guest's observation, returned as the completed output. The
// test asserts on Escaped (host isolation) and Code/RStatus (mediation).
type report struct {
	Status  string `json:"status"` // resume-result convention: always "completed" here
	Mode    string `json:"mode"`
	Escaped bool   `json:"escaped,omitempty"` // true if the guest reached a host resource (a breach)
	Detail  string `json:"detail,omitempty"`
	Code    string `json:"code,omitempty"`    // errno the host returned for a syscall
	RStatus string `json:"rstatus,omitempty"` // wire status the host returned
	Extra   string `json:"extra,omitempty"`
}

//go:wasmexport run
func run() int32 {
	var in input
	if err := pdk.InputJSON(&in); err != nil {
		pdk.SetError(fmt.Errorf("decode input: %w", err))
		return 1
	}

	out := report{Status: "completed", Mode: in.Mode}

	switch in.Mode {
	case "echo":
		out.RStatus, out.Code = mustDispatch(sys.Syscall{
			Abi: sys.ABIVersion, Name: "host.echo", Args: json.RawMessage(`{"v":1}`),
		})

	// ---- host isolation (law #1) ----

	case "fs_read":
		// No WASI preopens are configured, so opening any host path must fail.
		out.Escaped, out.Detail = tryFSRead()

	case "fs_write":
		out.Escaped, out.Detail = tryFSWrite()

	case "env":
		env := os.Environ()
		out.Escaped = len(env) > 0
		out.Detail = fmt.Sprintf("%d vars", len(env))
		if len(env) > 0 {
			out.Extra = fmt.Sprintf("%v", env)
		}

	case "args":
		// os.Args[0] is the module name; anything beyond it is host-supplied
		// argv the processor must not pass.
		args := os.Args
		out.Escaped = len(args) > 1
		out.Detail = fmt.Sprintf("%d args", len(args))
		if len(args) > 1 {
			out.Extra = fmt.Sprintf("%v", args[1:])
		}

	case "http":
		// Ambient network through extism:host/env http_request, bypassing the
		// syscall dispatcher. With no allowed_hosts the SDK denies it; the send
		// either traps the module (ResumeFailed) or returns a non-2xx/zero
		// status. Escaped only if a real fetch succeeded.
		out.Escaped, out.Detail = tryHTTP()

	// ---- the ABI trust boundary ----

	case "forge_abi":
		// A structurally valid envelope declaring an ABI the host does not speak.
		out.RStatus, out.Code = mustDispatch(sys.Syscall{
			Abi: sys.ABIVersion + 1, Name: "host.echo", Args: json.RawMessage(`{}`),
		})

	case "forge_garbage":
		// Bytes that are not an envelope in any encoding: rejected, never executed.
		out.RStatus, out.Code = dispatchRaw([]byte{0xff, 0xff, 0xff, 0xff, 0xff, 0xff, 0xff, 0xff})

	// ---- resource limits / host survival (law #1) ----

	case "dispatch_error":
		// The driver returns a Go (infrastructure) error. The host must trap
		// the quantum; the guest must never observe an outcome. Reaching the
		// line after the dispatch would prove journal-before-observe broken.
		rstatus, code := mustDispatch(sys.Syscall{
			Abi: sys.ABIVersion, Name: "host.boom", Args: json.RawMessage(`{}`),
		})
		out.Detail = "guest observed an unjournaled outcome"
		out.RStatus, out.Code = rstatus, code

	case "hog":
		var chunks [][]byte
		for i := 0; i < 8192; i++ {
			chunk := make([]byte, 1<<20)
			chunk[0] = byte(i)
			chunks = append(chunks, chunk)
		}
		pdk.SetErrorString(fmt.Sprintf("hog survived %d MiB", len(chunks)))
		return 1

	case "infinite":
		for {
		}

	case "ambient":
		// Reads the WASI clock and RNG the processor pins; two fresh processes
		// must observe identical values (determinism, law #2).
		buf := make([]byte, 16)
		// crypto/rand under wasip1 reads WASI random_get, which the processor pins.
		if _, err := crand.Read(buf); err != nil {
			pdk.SetError(err)
			return 1
		}
		out.Extra = fmt.Sprintf("time=%s rand=%s",
			time.Now().UTC().Format(time.RFC3339Nano), hex.EncodeToString(buf))

	default:
		pdk.SetErrorString("unknown mode: " + in.Mode)
		return 1
	}

	if err := pdk.OutputJSON(out); err != nil {
		pdk.SetError(err)
		return 1
	}
	return 0
}

// mustDispatch sends a syscall and reports what the host answered as the
// (status, errno) pair the test asserts on. Only a codec failure is fatal; a
// failed response is a valid observation.
func mustDispatch(sc sys.Syscall) (rstatus, code string) {
	encoded, err := json.Marshal(sc)
	if err != nil {
		pdk.SetError(fmt.Errorf("encode syscall: %w", err))
		return "unspecified", "guest_encode_error"
	}
	return dispatchRaw(encoded)
}

func dispatchRaw(payload []byte) (rstatus, code string) {
	mem := pdk.AllocateBytes(payload)
	defer mem.Free()
	off := hostSyscall(mem.Offset())

	var result sys.SyscallResult
	if err := json.Unmarshal(pdk.ParamBytes(off), &result); err != nil {
		pdk.SetError(fmt.Errorf("decode host response: %w", err))
		// A sentinel the test will not mistake for a real host status.
		return "unspecified", "guest_decode_error"
	}
	return statusName(result.Status()), string(result.Errno())
}

func statusName(s sys.SyscallStatus) string {
	if s == "" {
		return "unspecified"
	}
	return string(s)
}

func tryFSRead() (escaped bool, detail string) {
	defer func() {
		if r := recover(); r != nil {
			escaped, detail = false, fmt.Sprintf("panic: %v", r)
		}
	}()
	for _, path := range []string{"/etc/passwd", "/", ".", "/proc/self/environ"} {
		if data, err := os.ReadFile(path); err == nil {
			return true, fmt.Sprintf("read %d bytes from %s", len(data), path)
		}
		if entries, err := os.ReadDir(path); err == nil {
			return true, fmt.Sprintf("listed %d entries in %s", len(entries), path)
		}
	}
	return false, "all host reads refused"
}

func tryFSWrite() (escaped bool, detail string) {
	defer func() {
		if r := recover(); r != nil {
			escaped, detail = false, fmt.Sprintf("panic: %v", r)
		}
	}()
	for _, path := range []string{"/tmp/aurora_escape", "./aurora_escape", "/aurora_escape"} {
		if err := os.WriteFile(path, []byte("owned"), 0o644); err == nil {
			_ = os.Remove(path)
			return true, "wrote " + path
		}
	}
	return false, "all host writes refused"
}

func tryHTTP() (escaped bool, detail string) {
	defer func() {
		if r := recover(); r != nil {
			escaped, detail = false, fmt.Sprintf("panic: %v", r)
		}
	}()
	req := pdk.NewHTTPRequest(pdk.MethodGet, "https://example.com/")
	resp := req.Send()
	status := resp.Status()
	if status >= 200 && status < 400 {
		return true, fmt.Sprintf("fetched with status %d", status)
	}
	return false, fmt.Sprintf("no fetch (status %d)", status)
}

func main() {}
