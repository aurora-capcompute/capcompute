package sys

import (
	"bytes"
	"encoding/json"
	"strings"
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

// The durable rendering must carry guest bytes verbatim — no HTML escaping.
// A journal that stores \u003c for a guest's < would replay different bytes
// than the guest re-issues. MarshalJSON emits verbatim bytes; the outer
// encoder a store persists through must itself disable HTML escaping
// (json.Marshal's compact step would re-escape a Marshaler's output).
func TestSyscallResultMarshalKeepsHTMLCharactersVerbatim(t *testing.T) {
	raw := `{"text":"<done> & gone"}`
	var buf bytes.Buffer
	encoder := json.NewEncoder(&buf)
	encoder.SetEscapeHTML(false)
	if err := encoder.Encode(Result([]byte(raw))); err != nil {
		t.Fatal(err)
	}
	data := buf.Bytes()
	if !strings.Contains(string(data), raw) {
		t.Fatalf("marshaled result = %s, want verbatim %s", data, raw)
	}
	var decoded SyscallResult
	if err := json.Unmarshal(data, &decoded); err != nil {
		t.Fatal(err)
	}
	if string(decoded.Result()) != raw {
		t.Fatalf("round-tripped result = %s", decoded.Result())
	}
}
