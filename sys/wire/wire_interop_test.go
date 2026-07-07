package wire_test

import (
	"bytes"
	"encoding/hex"
	"testing"

	"google.golang.org/protobuf/proto"

	"github.com/aurora-capcompute/capcompute/sys/wire"
	"github.com/aurora-capcompute/capcompute/sys/wire/internal/refpb"
)

// The hand codec must interoperate with a real protobuf implementation in
// both directions. refpb is protoc-generated from envelope.proto — the
// reference the Rust program SDK's prost-style codec is also held to.

var syscallMatrix = []wire.Syscall{
	{},
	{Abi: 3, Name: "mail.send", Args: []byte(`{"to":"ops"}`)},
	{Abi: 3, Name: "sys.declassify"},
	{Abi: 300, Name: "x", Args: bytes.Repeat([]byte{0xFF}, 300)}, // multi-byte varints
}

var responseMatrix = []wire.Response{
	{},
	{Abi: 3, Status: wire.StatusResult, Result: []byte(`{"ok":true}`), Labels: []string{"syscall:mail.send"}},
	{Abi: 3, Status: wire.StatusYield, Message: "waiting for approval"},
	{Abi: 3, Status: wire.StatusFailed, Code: "denied", Message: "flow policy", Labels: []string{"a", "b", "c"}},
}

func TestSyscallInterop(t *testing.T) {
	for _, syscall := range syscallMatrix {
		// hand-encoded → protoc-decoded
		var decoded refpb.Syscall
		if err := proto.Unmarshal(wire.EncodeSyscall(syscall), &decoded); err != nil {
			t.Fatalf("protoc decode of hand encoding: %v", err)
		}
		if decoded.GetAbi() != syscall.Abi || decoded.GetName() != syscall.Name || !bytes.Equal(decoded.GetArgs(), syscall.Args) {
			t.Fatalf("protoc saw %+v, hand codec sent %+v", &decoded, syscall)
		}

		// protoc-encoded → hand-decoded
		reference, err := proto.Marshal(&refpb.Syscall{Abi: syscall.Abi, Name: syscall.Name, Args: syscall.Args})
		if err != nil {
			t.Fatalf("protoc encode: %v", err)
		}
		roundTripped, err := wire.DecodeSyscall(reference)
		if err != nil {
			t.Fatalf("hand decode of protoc encoding: %v", err)
		}
		if roundTripped.Abi != syscall.Abi || roundTripped.Name != syscall.Name || !bytes.Equal(roundTripped.Args, syscall.Args) {
			t.Fatalf("hand codec saw %+v, protoc sent %+v", roundTripped, syscall)
		}
	}
}

func TestResponseInterop(t *testing.T) {
	for _, response := range responseMatrix {
		var decoded refpb.Response
		if err := proto.Unmarshal(wire.EncodeResponse(response), &decoded); err != nil {
			t.Fatalf("protoc decode of hand encoding: %v", err)
		}
		if decoded.GetAbi() != response.Abi || uint32(decoded.GetStatus()) != uint32(response.Status) ||
			decoded.GetCode() != response.Code || !bytes.Equal(decoded.GetResult(), response.Result) ||
			decoded.GetMessage() != response.Message || !equalLabels(decoded.GetLabels(), response.Labels) {
			t.Fatalf("protoc saw %+v, hand codec sent %+v", &decoded, response)
		}

		reference, err := proto.Marshal(&refpb.Response{
			Abi:     response.Abi,
			Status:  refpb.Status(response.Status),
			Code:    response.Code,
			Result:  response.Result,
			Message: response.Message,
			Labels:  response.Labels,
		})
		if err != nil {
			t.Fatalf("protoc encode: %v", err)
		}
		roundTripped, err := wire.DecodeResponse(reference)
		if err != nil {
			t.Fatalf("hand decode of protoc encoding: %v", err)
		}
		if roundTripped.Abi != response.Abi || roundTripped.Status != response.Status ||
			roundTripped.Code != response.Code || !bytes.Equal(roundTripped.Result, response.Result) ||
			roundTripped.Message != response.Message || !equalLabels(roundTripped.Labels, response.Labels) {
			t.Fatalf("hand codec saw %+v, protoc sent %+v", roundTripped, response)
		}
	}
}

// Golden fixtures shared verbatim with the Rust program SDK's codec tests
// (aurora-brains sdk/src/wire.rs) — the cross-language pin.
func TestGoldenFixtures(t *testing.T) {
	syscallGold := "0803" + "1209" + hex.EncodeToString([]byte("mail.send")) +
		"1a0c" + hex.EncodeToString([]byte(`{"to":"ops"}`))
	if got := hex.EncodeToString(wire.EncodeSyscall(wire.Syscall{Abi: 3, Name: "mail.send", Args: []byte(`{"to":"ops"}`)})); got != syscallGold {
		t.Fatalf("syscall fixture drifted:\n got %s\nwant %s", got, syscallGold)
	}

	responseGold := "0803" + "1003" + "1a06" + hex.EncodeToString([]byte("denied")) +
		"2a02" + hex.EncodeToString([]byte("no")) + "3201" + hex.EncodeToString([]byte("a")) + "3201" + hex.EncodeToString([]byte("b"))
	if got := hex.EncodeToString(wire.EncodeResponse(wire.Response{
		Abi: 3, Status: wire.StatusFailed, Code: "denied", Message: "no", Labels: []string{"a", "b"},
	})); got != responseGold {
		t.Fatalf("response fixture drifted:\n got %s\nwant %s", got, responseGold)
	}
}

// A future schema may add fields; today's decoder must skip them.
func TestUnknownFieldsSkipped(t *testing.T) {
	message := wire.EncodeSyscall(wire.Syscall{Abi: 3, Name: "mail.send"})
	// Unknown field 9 (varint), field 10 (bytes), field 11 (i64), field 12 (i32).
	message = append(message, 0x48, 0x2A)
	message = append(message, 0x52, 0x03, 'f', 'o', 'o')
	message = append(message, 0x59, 1, 2, 3, 4, 5, 6, 7, 8)
	message = append(message, 0x65, 1, 2, 3, 4)

	decoded, err := wire.DecodeSyscall(message)
	if err != nil {
		t.Fatalf("decode with unknown fields: %v", err)
	}
	if decoded.Abi != 3 || decoded.Name != "mail.send" {
		t.Fatalf("known fields corrupted by unknown ones: %+v", decoded)
	}
}

func TestDecodeRejectsGarbage(t *testing.T) {
	for name, data := range map[string][]byte{
		"json (abi v2)":    []byte(`{"abi":2,"name":"x"}`),
		"truncated length": {0x12, 0x09, 'm'},
		"truncated varint": {0x08},
	} {
		if _, err := wire.DecodeSyscall(data); err == nil {
			t.Fatalf("%s: decode accepted garbage", name)
		}
	}
}

func equalLabels(left, right []string) bool {
	if len(left) != len(right) {
		return false
	}
	for i := range left {
		if left[i] != right[i] {
			return false
		}
	}
	return true
}
