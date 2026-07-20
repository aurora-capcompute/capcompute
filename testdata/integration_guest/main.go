//go:build wasip1

// Command integration_guest is the smallest real Extism guest used by
// capcompute's integration tests. It is built with the standard Go toolchain
// (GOOS=wasip1 GOARCH=wasm go build -buildmode=c-shared), the same `go` that
// runs the tests, so the integration proofs run anywhere `go` does — no extra
// toolchain, no silent skip.
package main

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"time"

	"github.com/extism/go-pdk"

	"github.com/aurora-capcompute/capcompute/sys"
)

//go:wasmimport extism:host/compute syscall
func hostSyscall(uint64) uint64

type input struct {
	Mode string `json:"mode"`
}

type output struct {
	Status      string          `json:"status"`
	Observation json.RawMessage `json:"observation,omitempty"`
}

//go:wasmexport run
func run() int32 {
	var in input
	if err := pdk.InputJSON(&in); err != nil {
		pdk.SetError(fmt.Errorf("decode input: %w", err))
		return 1
	}

	switch in.Mode {
	case "completed":
		result, err := dispatch(sys.Syscall{Name: "host.echo", Args: json.RawMessage(`{"value":"ok"}`)})
		if err != nil {
			pdk.SetError(err)
			return 1
		}
		if result.Status() != sys.StatusResult {
			pdk.SetErrorString("expected result status")
			return 1
		}
		if err := pdk.OutputJSON(output{Status: "completed", Observation: result.Result()}); err != nil {
			pdk.SetError(err)
			return 1
		}
		return 0

	case "yielded":
		result, err := dispatch(sys.Syscall{Name: "host.yield"})
		if err != nil {
			pdk.SetError(err)
			return 1
		}
		if result.Status() != sys.StatusYield {
			pdk.SetErrorString("expected yield status")
			return 1
		}
		if err := pdk.OutputJSON(output{Status: "yielded"}); err != nil {
			pdk.SetError(err)
			return 1
		}
		return 0

	case "failed":
		pdk.SetErrorString("guest requested failure")
		return 1

	// infra dispatches a syscall whose driver errors — a machinery error, not
	// a failed result. The host traps the quantum (the outcome was never
	// journaled, so the guest must not observe it); control must never return
	// here. Completing with the observed status would prove the law broken.
	case "infra":
		result, err := dispatch(sys.Syscall{Name: "host.missing"})
		observation, _ := json.Marshal(fmt.Sprintf("status=%s err=%v", result.Status(), err))
		if err := pdk.OutputJSON(output{Status: "completed", Observation: observation}); err != nil {
			pdk.SetError(err)
			return 1
		}
		return 0

	case "infinite":
		for {
		}

	// hog allocates far past any sane memory cap; under MaxMemoryPages the
	// growth traps and the resume must report failure, never a host OOM.
	case "hog":
		var chunks [][]byte
		for i := 0; i < 4096; i++ {
			chunk := make([]byte, 1<<20)
			chunk[0] = byte(i)
			chunks = append(chunks, chunk)
		}
		pdk.SetErrorString(fmt.Sprintf("hog survived %d MiB", len(chunks)))
		return 1

	// ambient reads the WASI clock and RNG — the sources the processor pins.
	// The host test runs this mode on two fresh processes and requires
	// identical observations (law #2: determinism).
	case "ambient":
		buf := make([]byte, 16)
		if _, err := rand.Read(buf); err != nil {
			pdk.SetError(fmt.Errorf("random_get: %w", err))
			return 1
		}
		now := time.Now().UTC()
		observation, err := json.Marshal(map[string]string{
			"time": now.Format(time.RFC3339Nano),
			"rand": hex.EncodeToString(buf),
		})
		if err != nil {
			pdk.SetError(err)
			return 1
		}
		if err := pdk.OutputJSON(output{Status: "completed", Observation: observation}); err != nil {
			pdk.SetError(err)
			return 1
		}
		return 0

	// http attempts ambient network through extism:host/env http_request,
	// bypassing the syscall dispatcher. The processor refuses images with
	// allowed_hosts, so this must fail (law #1: no ambient authority).
	case "http":
		request := pdk.NewHTTPRequest(pdk.MethodGet, "https://example.com")
		response := request.Send()
		pdk.SetErrorString(fmt.Sprintf("ambient http succeeded with status %d", response.Status()))
		return 1

	default:
		pdk.SetErrorString("unknown mode")
		return 1
	}
}

// The wasip1 c-shared build needs a main symbol; the module's real entry
// point is the exported run above.
func main() {}

func dispatch(sc sys.Syscall) (sys.SyscallResult, error) {
	sc.Abi = sys.ABIVersion
	encoded, err := json.Marshal(sc)
	if err != nil {
		return sys.SyscallResult{}, fmt.Errorf("encode syscall: %w", err)
	}
	request := pdk.AllocateBytes(encoded)
	defer request.Free()

	responseOffset := hostSyscall(request.Offset())
	var result sys.SyscallResult
	if err := json.Unmarshal(pdk.ParamBytes(responseOffset), &result); err != nil {
		return sys.SyscallResult{}, fmt.Errorf("decode host response: %w", err)
	}
	if result.Status() == sys.StatusFailed {
		return sys.SyscallResult{}, fmt.Errorf("host failed (%s): %s", result.Errno(), result.Message())
	}
	return result, nil
}
