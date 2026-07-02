//go:build tinygo

package main

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"time"

	"github.com/extism/go-pdk"
)

//go:wasmimport extism:host/compute syscall
func hostSyscall(uint64) uint64

const abiVersion = 2

type input struct {
	Mode string `json:"mode"`
}

type syscall struct {
	Abi  int             `json:"abi"`
	Name string          `json:"name"`
	Args json.RawMessage `json:"args,omitempty"`
}

type hostResponse struct {
	Abi     int             `json:"abi"`
	Status  string          `json:"status"`
	Code    string          `json:"code,omitempty"`
	Result  json.RawMessage `json:"result,omitempty"`
	Message string          `json:"message,omitempty"`
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
		response, err := dispatch(syscall{Name: "host.echo", Args: json.RawMessage(`{"value":"ok"}`)})
		if err != nil {
			pdk.SetError(err)
			return 1
		}
		if response.Status != "result" {
			pdk.SetErrorString("expected result status")
			return 1
		}
		if err := pdk.OutputJSON(output{Status: "completed", Observation: response.Result}); err != nil {
			pdk.SetError(err)
			return 1
		}
		return 0

	case "yielded":
		response, err := dispatch(syscall{Name: "host.yield"})
		if err != nil {
			pdk.SetError(err)
			return 1
		}
		if response.Status != "yield" {
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

	// ambient reads the WASI clock and RNG — the sources the kernel pins.
	// The host test runs this mode on two fresh processes and requires
	// identical observations (kernel law #2: determinism).
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
	// bypassing the syscall dispatcher. The kernel refuses images with
	// allowed_hosts, so this must fail (kernel law #1: no ambient authority).
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

func dispatch(sc syscall) (hostResponse, error) {
	sc.Abi = abiVersion
	data, err := json.Marshal(sc)
	if err != nil {
		return hostResponse{}, fmt.Errorf("encode syscall: %w", err)
	}

	request := pdk.AllocateBytes(data)
	defer request.Free()

	responseOffset := hostSyscall(request.Offset())
	var response hostResponse
	if err := pdk.JSONFrom(responseOffset, &response); err != nil {
		return hostResponse{}, fmt.Errorf("decode host response: %w", err)
	}
	if response.Status == "failed" {
		return hostResponse{}, fmt.Errorf("host failed (%s): %s", response.Code, response.Message)
	}
	return response, nil
}
