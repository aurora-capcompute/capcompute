package capcompute

import (
	"context"
	"encoding/json"
	"fmt"

	extism "github.com/extism/go-sdk"

	"github.com/aurora-capcompute/capcompute/sys"
)

type pidContextKey struct{}

type hostResponse struct {
	Abi     int               `json:"abi"`
	Status  sys.SyscallStatus `json:"status"`
	Code    sys.Errno         `json:"code,omitempty"`
	Result  json.RawMessage   `json:"result,omitempty"`
	Message string            `json:"message,omitempty"`
	// Labels is the result's provenance (sorted source classes). Guests see
	// where their data came from — a brain can treat untrusted content as
	// untrusted instead of trusting whatever it reads.
	Labels []string `json:"labels,omitempty"`
}

func failResponse(errno sys.Errno, message string) hostResponse {
	return hostResponse{Abi: sys.ABIVersion, Status: sys.StatusFailed, Code: errno, Message: message}
}

// hostFunction registers the single syscall entry point. Guests import
// `extism:host/compute syscall`; every host capability flows through it.
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

	var syscall sys.Syscall
	if err := json.Unmarshal(rawSyscall, &syscall); err != nil {
		return returnToGuest(plugin, failResponse(sys.ErrnoInvalidArgs, fmt.Errorf("decode syscall: %w", err).Error()))
	}
	if syscall.Abi != sys.ABIVersion {
		return returnToGuest(plugin, failResponse(sys.ErrnoBadABI,
			fmt.Sprintf("syscall abi %d, host speaks %d", syscall.Abi, sys.ABIVersion)))
	}

	result, err := process.dispatcher.Dispatch(ctx, process.Cred, syscall, sys.Authorization{})
	if err != nil {
		return returnToGuest(plugin, failResponse(sys.ErrnoInternal, err.Error()))
	}

	return returnToGuest(plugin, hostResponse{
		Abi:     sys.ABIVersion,
		Status:  result.Status(),
		Code:    result.Errno(),
		Result:  result.Result(),
		Message: result.Message(),
		Labels:  result.Labels(),
	})
}

func returnToGuest(plugin *extism.CurrentPlugin, response hostResponse) uint64 {
	data, err := json.Marshal(response)
	if err != nil {
		panic(fmt.Errorf("encode host response: %w", err))
	}

	offset, err := plugin.WriteBytes(data)
	if err != nil {
		panic(fmt.Errorf("write host response: %w", err))
	}
	return offset
}
