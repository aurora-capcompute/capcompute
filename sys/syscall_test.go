package sys

import (
	"encoding/json"
	"testing"
)

func TestSyscallResultJSONRoundTrip(t *testing.T) {
	for name, result := range map[string]SyscallResult{
		"result":  Result(json.RawMessage(`{"ok":true}`)),
		"failed":  FailCode(ErrnoDenied, "permission denied"),
		"yield":   Yield("waiting"),
		"nil-out": Result(nil),
	} {
		t.Run(name, func(t *testing.T) {
			data, err := json.Marshal(result)
			if err != nil {
				t.Fatalf("marshal: %v", err)
			}
			var decoded SyscallResult
			if err := json.Unmarshal(data, &decoded); err != nil {
				t.Fatalf("unmarshal: %v", err)
			}
			if decoded.Status() != result.Status() || decoded.Errno() != result.Errno() ||
				decoded.Message() != result.Message() || string(decoded.Result()) != string(result.Result()) {
				t.Fatalf("round trip changed the result: %#v -> %#v", result, decoded)
			}
		})
	}
}
