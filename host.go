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

func dispatchSyscall(
	ctx context.Context,
	plugin *extism.CurrentPlugin,
	offset uint64,
) uint64 {
	dispatch, ok := ctx.Value(syscallContextKey{}).(syscallFunc)
	if !ok || dispatch == nil {
		return returnToGuest(plugin, sys.FailCode(sys.ErrnoInternal, "process missing from context"))
	}
	rawSyscall, err := plugin.ReadBytes(offset)
	if err != nil {
		return returnToGuest(plugin, sys.FailCode(sys.ErrnoInvalidArgs, fmt.Errorf("read raw syscall: %w", err).Error()))
	}

	var syscall sys.Syscall
	if err := json.Unmarshal(rawSyscall, &syscall); err != nil {
		return returnToGuest(plugin, sys.FailCode(sys.ErrnoInvalidArgs, fmt.Errorf("decode syscall: %w", err).Error()))
	}
	if syscall.Abi != sys.ABIVersion {
		return returnToGuest(plugin, sys.FailCode(sys.ErrnoBadABI,
			fmt.Sprintf("syscall abi %d, host speaks %d", syscall.Abi, sys.ABIVersion)))
	}

	result, err := dispatch(ctx, syscall)
	if err != nil {
		// An infrastructure error is not an outcome: nothing was journaled, so
		// the guest must not observe it (journal-before-observe covers the
		// indeterminate case too). Trap the quantum — the panic surfaces as a
		// failed resume, the intent stays open in the journal, and the re-drive
		// resolves it under the original idempotency key. A handler-level
		// failure (a failed SyscallResult) still flows to the guest as a
		// recoverable observation; only the machinery's own errors trap.
		panic(fmt.Errorf("dispatch %s: %w", syscall.Name, err))
	}
	return returnToGuest(plugin, result)
}

func returnToGuest(plugin *extism.CurrentPlugin, result sys.SyscallResult) uint64 {
	encoded, err := json.Marshal(result)
	if err != nil {
		panic(fmt.Errorf("encode host response: %w", err))
	}
	offset, err := plugin.WriteBytes(encoded)
	if err != nil {
		panic(fmt.Errorf("write host response: %w", err))
	}
	return offset
}
