package capcompute

import (
	"context"
	"encoding/json"
	"fmt"

	extism "github.com/extism/go-sdk"

	"github.com/aurora-capcompute/capcompute/sys"
)

type syscallContextKey struct{}

// syscallFunc is one process's dispatch, already bound to its credential and
// dispatcher. Resume plants it in the call context, which is the whole of what
// the host function needs to know about the process that was given the CPU —
// so nothing here is generic over the credential type.
type syscallFunc func(context.Context, sys.Syscall) (sys.SyscallResult, error)

// hostFunction registers the single syscall entry point. Guests import
// `extism:host/compute syscall`; every host capability flows through it,
// encoded as the ABI v4 JSON envelope — a sys.Syscall in, a sys.SyscallResult
// out, the same types and the same JSON the dispatcher chain and the journal
// already speak, so nothing is translated at the boundary.
func hostFunction() extism.HostFunction {
	host := extism.NewHostFunctionWithStack(
		"syscall",
		func(ctx context.Context, plugin *extism.CurrentPlugin, stack []uint64) {
			stack[0] = dispatchSyscall(ctx, plugin, stack[0])
		},
		[]extism.ValueType{extism.ValueTypePTR},
		[]extism.ValueType{extism.ValueTypePTR},
	)
	host.SetNamespace("extism:host/compute")
	return host
}

// dispatchSyscall serves one guest syscall: it reads the envelope from guest
// memory, checks the ABI version, dispatches, and writes the result back. A
// malformed envelope is a failed result the guest can react to.
func dispatchSyscall(ctx context.Context, plugin *extism.CurrentPlugin, offset uint64) uint64 {
	// Resume planted the process's dispatch. The host function runs only inside
	// a Resume.
	dispatch := ctx.Value(syscallContextKey{}).(syscallFunc)
	rawSyscall, err := plugin.ReadBytes(offset)
	if err != nil {
		return returnToGuest(plugin, sys.FailCode(sys.ErrnoInvalidArgs, "read raw syscall: "+err.Error()))
	}

	var syscall sys.Syscall
	if err := json.Unmarshal(rawSyscall, &syscall); err != nil {
		return returnToGuest(plugin, sys.FailCode(sys.ErrnoInvalidArgs, "decode syscall: "+err.Error()))
	}
	if syscall.Abi != sys.ABIVersion {
		return returnToGuest(plugin, sys.FailCode(sys.ErrnoBadABI,
			fmt.Sprintf("syscall abi %d, host speaks %d", syscall.Abi, sys.ABIVersion)))
	}

	result, err := dispatch(ctx, syscall)
	if err != nil {
		// An error (as opposed to a failed result) is the machinery failing, not
		// an outcome: nothing was journaled, so the guest must not observe it.
		// Trap the quantum. The panic surfaces as a failed resume, the intent
		// stays open in the journal, and the next run drives it again. A failed
		// result still reaches the guest as a recoverable observation.
		panic(fmt.Errorf("dispatch %s: %w", syscall.Name, err))
	}
	return returnToGuest(plugin, result)
}

// guestResult is the processor's guest-facing rendering of a syscall outcome,
// and it is a distinct type from the durable one on purpose.
//
// A SyscallResult also carries provenance labels: the runtime's flow monitor
// stamps them and the journal records them, so a crash-restarted host can
// rebuild a run's taint. A guest must never see them. It is opaque and possibly
// adversarial, and the labels are exactly the map it would need to route around
// the flow policy — which of its calls taint the run, and therefore which path
// out is still clean.
//
// Expressing that as its own struct rather than as a field the marshaller skips
// means the guest ABI structurally cannot carry a label: there is no field to
// forget to strip.
type guestResult struct {
	Status  sys.SyscallStatus `json:"status"`
	Code    sys.Errno         `json:"code,omitempty"`
	Result  json.RawMessage   `json:"result,omitempty"`
	Message string            `json:"message,omitempty"`
}

// returnToGuest writes a result into guest memory and returns its offset.
func returnToGuest(plugin *extism.CurrentPlugin, result sys.SyscallResult) uint64 {
	encoded, err := json.Marshal(guestResult{
		Status:  result.Status(),
		Code:    result.Errno(),
		Result:  result.Result(),
		Message: result.Message(),
	})
	if err != nil {
		panic(fmt.Errorf("encode host response: %w", err))
	}
	offset, err := plugin.WriteBytes(encoded)
	if err != nil {
		panic(fmt.Errorf("write host response: %w", err))
	}
	return offset
}
