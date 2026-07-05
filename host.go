package capcompute

import (
	"context"
	"fmt"

	extism "github.com/extism/go-sdk"

	"github.com/aurora-capcompute/capcompute/sys"
	"github.com/aurora-capcompute/capcompute/sys/wire"
)

type pidContextKey struct{}

func failResponse(errno sys.Errno, message string) wire.Response {
	return wire.Response{Abi: sys.ABIVersion, Status: wire.StatusFailed, Code: string(errno), Message: message}
}

// hostFunction registers the single syscall entry point. Guests import
// `extism:host/compute syscall`; every host capability flows through it,
// encoded as the ABI v3 protobuf envelope (sys/wire).
func hostFunction[ID comparable, K PID[ID]](table ProcessTable[ID, K]) extism.HostFunction {
	host := extism.NewHostFunctionWithStack(
		"syscall",
		func(ctx context.Context, plugin *extism.CurrentPlugin, stack []uint64) {
			stack[0] = dispatchSyscall(ctx, table, plugin, stack[0])
		},
		[]extism.ValueType{extism.ValueTypePTR},
		[]extism.ValueType{extism.ValueTypePTR},
	)
	host.SetNamespace("extism:host/compute")
	return host
}

func dispatchSyscall[ID comparable, K PID[ID]](
	ctx context.Context,
	table ProcessTable[ID, K],
	plugin *extism.CurrentPlugin,
	offset uint64,
) uint64 {
	pid, ok := ctx.Value(pidContextKey{}).(ID)
	if !ok {
		return returnToGuest(plugin, failResponse(sys.ErrnoInternal, "pid missing from context"))
	}
	process, err := table.LoadProcess(ctx, pid)
	if err != nil {
		return returnToGuest(plugin, failResponse(sys.ErrnoNotFound, "process not found"))
	}
	rawSyscall, err := plugin.ReadBytes(offset)
	if err != nil {
		return returnToGuest(plugin, failResponse(sys.ErrnoInvalidArgs, fmt.Errorf("read raw syscall: %w", err).Error()))
	}

	// A JSON envelope is the pre-v3 wire — classify it as the version
	// mismatch it is, not as garbage.
	if len(rawSyscall) > 0 && rawSyscall[0] == '{' {
		return returnToGuest(plugin, failResponse(sys.ErrnoBadABI,
			fmt.Sprintf("JSON envelope is pre-v3; host speaks %d (protobuf)", sys.ABIVersion)))
	}
	decoded, err := wire.DecodeSyscall(rawSyscall)
	if err != nil {
		return returnToGuest(plugin, failResponse(sys.ErrnoInvalidArgs, fmt.Errorf("decode syscall: %w", err).Error()))
	}
	if decoded.Abi != sys.ABIVersion {
		return returnToGuest(plugin, failResponse(sys.ErrnoBadABI,
			fmt.Sprintf("syscall abi %d, host speaks %d", decoded.Abi, sys.ABIVersion)))
	}

	result, err := process.dispatcher.Dispatch(ctx, process.Cred, wire.ToSyscall(decoded), sys.Authorization{})
	if err != nil {
		// An infrastructure error is not an outcome: nothing was journaled, so
		// the guest must not observe it (journal-before-observe covers the
		// indeterminate case too). Trap the quantum — the panic surfaces as a
		// failed resume, the intent stays open in the journal, and the re-drive
		// resolves it under the original idempotency key. A handler-level
		// failure (a failed SyscallResult) still flows to the guest as a
		// recoverable observation; only the machinery's own errors trap.
		panic(fmt.Errorf("dispatch %s: %w", decoded.Name, err))
	}
	return returnToGuest(plugin, wire.FromResult(result))
}

func returnToGuest(plugin *extism.CurrentPlugin, response wire.Response) uint64 {
	offset, err := plugin.WriteBytes(wire.EncodeResponse(response))
	if err != nil {
		panic(fmt.Errorf("write host response: %w", err))
	}
	return offset
}
