package capcompute

import (
	"capcompute/dispatcher"
	"context"
	"encoding/json"
	"fmt"

	extism "github.com/extism/go-sdk"
)

type sessionKeyContextKey struct{}

type hostResponse struct {
	Status  dispatcher.OutcomeKind `json:"status"`
	Result  json.RawMessage        `json:"result,omitempty"`
	Message string                 `json:"message,omitempty"`
}

func hostFunction[ID comparable, K SessionKey[ID]](store SessionStore[ID, K]) extism.HostFunction {
	host := extism.NewHostFunctionWithStack(
		"play",
		func(ctx context.Context, plugin *extism.CurrentPlugin, stack []uint64) {
			stack[0] = dispatchHostCall(ctx, store, plugin, stack[0])
		},
		[]extism.ValueType{extism.ValueTypePTR},
		[]extism.ValueType{extism.ValueTypePTR},
	)
	host.SetNamespace("extism:host/compute")
	return host
}

func dispatchHostCall[ID comparable, K SessionKey[ID]](
	ctx context.Context,
	store SessionStore[ID, K],
	plugin *extism.CurrentPlugin,
	offset uint64,
) uint64 {
	sessionKey, ok := ctx.Value(sessionKeyContextKey{}).(ID)
	if !ok {
		return returnToGuest(plugin, hostResponse{Status: dispatcher.OutcomeFailed, Message: "session key missing from context"})
	}
	session, err := store.LoadSession(ctx, sessionKey)
	if err != nil {
		return returnToGuest(plugin, hostResponse{Status: dispatcher.OutcomeFailed, Message: "session not found"})
	}
	rawCall, err := plugin.ReadBytes(offset)
	if err != nil {
		return returnToGuest(plugin, hostResponse{Status: dispatcher.OutcomeFailed, Message: fmt.Errorf("read raw call: %w", err).Error()})
	}

	var call dispatcher.Call
	if err := json.Unmarshal(rawCall, &call); err != nil {
		return returnToGuest(plugin, hostResponse{Status: dispatcher.OutcomeFailed, Message: fmt.Errorf("decode call: %w", err).Error()})
	}

	outcome, err := session.dispatcher.Dispatch(ctx, session.GuestData, call)
	if err != nil {
		return returnToGuest(plugin, hostResponse{
			Status:  dispatcher.OutcomeFailed,
			Message: err.Error(),
		})
	}

	return returnToGuest(plugin, hostResponse{
		Status:  outcome.Kind(),
		Result:  outcome.Result(),
		Message: outcome.Message(),
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
