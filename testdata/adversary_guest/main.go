//go:build wasip1

// Command adversary_guest is a deliberately hostile Extism guest used by
// capcompute's security tests. Unlike testdata/integration_guest (which needs
// TinyGo), this program is built with the standard Go toolchain
// (GOOS=wasip1 GOARCH=wasm go build -buildmode=c-shared), so the adversarial
// proofs run in any environment that already has `go` — no extra toolchain,
// no silent skip.
//
// Every mode attempts to break one of the two guarantees the kernel makes to
// the host that embeds it:
//
//   - host isolation (kernel law #1): the guest must not be able to touch the
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
	"fmt"
	"os"
	"time"

	"github.com/extism/go-pdk"

	"github.com/aurora-capcompute/capcompute/sys/wire"
)

//go:wasmimport extism:host/compute syscall
func hostSyscall(uint64) uint64

const abiVersion = 3

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
		resp := mustDispatch(wire.Syscall{Abi: abiVersion, Name: "host.echo", Args: []byte(`{"v":1}`)})
		out.RStatus, out.Code = statusName(resp.Status), resp.Code

	// ---- host isolation (kernel law #1) ----

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
		// argv the kernel must not pass.
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
		// A structurally valid envelope declaring the wrong ABI version.
		resp := mustDispatch(wire.Syscall{Abi: 2, Name: "host.echo", Args: []byte(`{}`)})
		out.RStatus, out.Code = statusName(resp.Status), resp.Code

	case "forge_json":
		// A JSON envelope: not the wire format, so the decoder must refuse it
		// rather than execute.
		resp := dispatchRaw([]byte(`{"abi":3,"name":"host.echo","args":{}}`))
		out.RStatus, out.Code = statusName(resp.Status), resp.Code

	case "forge_garbage":
		// Non-protobuf, non-JSON bytes: must be rejected, never executed.
		resp := dispatchRaw([]byte{0xff, 0xff, 0xff, 0xff, 0xff, 0xff, 0xff, 0xff})
		out.RStatus, out.Code = statusName(resp.Status), resp.Code

	case "probe_syscall":
		// Used with a process that is NOT in the process table: the host must
		// answer not_found, never route to a driver.
		resp := mustDispatch(wire.Syscall{Abi: abiVersion, Name: "host.echo", Args: []byte(`{}`)})
		out.RStatus, out.Code = statusName(resp.Status), resp.Code

	// ---- resource limits / host survival (kernel law #1) ----

	case "dispatch_error":
		// The driver returns a Go (infrastructure) error. The host must trap
		// the quantum; the guest must never observe an outcome. Reaching the
		// line after the dispatch would prove journal-before-observe broken.
		resp := mustDispatch(wire.Syscall{Abi: abiVersion, Name: "host.boom", Args: []byte(`{}`)})
		out.Detail = "guest observed an unjournaled outcome"
		out.RStatus, out.Code = statusName(resp.Status), resp.Code

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
		// Reads the WASI clock and RNG the kernel pins; two fresh processes
		// must observe identical values (determinism, kernel law #2).
		buf := make([]byte, 16)
		// crypto/rand under wasip1 reads WASI random_get, which the kernel pins.
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

// mustDispatch sends a syscall and returns the host response, treating only a
// decode failure as fatal (a StatusFailed response is a valid observation).
func mustDispatch(sc wire.Syscall) wire.Response {
	return dispatchRaw(wire.EncodeSyscall(sc))
}

func dispatchRaw(payload []byte) wire.Response {
	mem := pdk.AllocateBytes(payload)
	defer mem.Free()
	off := hostSyscall(mem.Offset())
	resp, err := wire.DecodeResponse(pdk.ParamBytes(off))
	if err != nil {
		pdk.SetError(fmt.Errorf("decode host response: %w", err))
		// Return a sentinel the test will not mistake for a real status.
		return wire.Response{Status: wire.StatusUnspecified, Code: "guest_decode_error"}
	}
	return resp
}

func statusName(s wire.Status) string {
	switch s {
	case wire.StatusResult:
		return "result"
	case wire.StatusYield:
		return "yield"
	case wire.StatusFailed:
		return "failed"
	default:
		return "unspecified"
	}
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
