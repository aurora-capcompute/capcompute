// Package wire is the ABI v3 envelope codec: the messages of envelope.proto
// in proto3 wire format, hand-rolled and reflection-free. Both sides of the
// trap boundary use it — the host in host.go, a Go guest program — so it
// depends on nothing (TinyGo-safe, minimal TCB). Interoperability with real
// protobuf implementations is pinned by wire_interop_test.go against
// protoc-generated reference code; unknown fields are skipped on decode, so
// the schema can grow without breaking old decoders.
package wire

import (
	"errors"
	"fmt"

	"github.com/aurora-capcompute/capcompute/sys"
)

// Syscall mirrors aurora.sys.v3.Syscall.
type Syscall struct {
	Abi  uint32
	Name string
	Args []byte
}

// Status mirrors aurora.sys.v3.Status.
type Status uint32

const (
	StatusUnspecified Status = 0
	StatusResult      Status = 1
	StatusYield       Status = 2
	StatusFailed      Status = 3
)

// Response mirrors aurora.sys.v3.Response.
type Response struct {
	Abi     uint32
	Status  Status
	Code    string
	Result  []byte
	Message string
	Labels  []string
}

// Field numbers from envelope.proto. Never reuse a number.
const (
	syscallAbi  = 1
	syscallName = 2
	syscallArgs = 3

	responseAbi     = 1
	responseStatus  = 2
	responseCode    = 3
	responseResult  = 4
	responseMessage = 5
	responseLabels  = 6
)

func EncodeSyscall(syscall Syscall) []byte {
	b := make([]byte, 0, 16+len(syscall.Name)+len(syscall.Args))
	b = appendVarintField(b, syscallAbi, uint64(syscall.Abi))
	b = appendBytesField(b, syscallName, []byte(syscall.Name))
	b = appendBytesField(b, syscallArgs, syscall.Args)
	return b
}

func DecodeSyscall(data []byte) (Syscall, error) {
	var syscall Syscall
	err := walkFields(data, func(field uint64, wireType uint64, payload []byte, value uint64) error {
		switch field {
		case syscallAbi:
			if wireType != wireVarint {
				return fmt.Errorf("syscall field %d: unexpected wire type %d", field, wireType)
			}
			syscall.Abi = uint32(value)
		case syscallName:
			if wireType != wireBytes {
				return fmt.Errorf("syscall field %d: unexpected wire type %d", field, wireType)
			}
			syscall.Name = string(payload)
		case syscallArgs:
			if wireType != wireBytes {
				return fmt.Errorf("syscall field %d: unexpected wire type %d", field, wireType)
			}
			syscall.Args = append([]byte(nil), payload...)
		}
		return nil
	})
	return syscall, err
}

func EncodeResponse(response Response) []byte {
	b := make([]byte, 0, 24+len(response.Result)+len(response.Message))
	b = appendVarintField(b, responseAbi, uint64(response.Abi))
	b = appendVarintField(b, responseStatus, uint64(response.Status))
	b = appendBytesField(b, responseCode, []byte(response.Code))
	b = appendBytesField(b, responseResult, response.Result)
	b = appendBytesField(b, responseMessage, []byte(response.Message))
	for _, label := range response.Labels {
		b = appendBytesField(b, responseLabels, []byte(label))
	}
	return b
}

func DecodeResponse(data []byte) (Response, error) {
	var response Response
	err := walkFields(data, func(field uint64, wireType uint64, payload []byte, value uint64) error {
		switch field {
		case responseAbi:
			if wireType != wireVarint {
				return fmt.Errorf("response field %d: unexpected wire type %d", field, wireType)
			}
			response.Abi = uint32(value)
		case responseStatus:
			if wireType != wireVarint {
				return fmt.Errorf("response field %d: unexpected wire type %d", field, wireType)
			}
			response.Status = Status(value)
		case responseCode:
			response.Code = string(payload)
		case responseResult:
			response.Result = append([]byte(nil), payload...)
		case responseMessage:
			response.Message = string(payload)
		case responseLabels:
			response.Labels = append(response.Labels, string(payload))
		}
		return nil
	})
	return response, err
}

// Host-side bridges between the wire messages and the kernel's sys types.
// Guests never need these; they work with the wire structs directly.

func ToSyscall(syscall Syscall) sys.Syscall {
	return sys.Syscall{Abi: int(syscall.Abi), Name: syscall.Name, Args: syscall.Args}
}

func FromResult(result sys.SyscallResult) Response {
	response := Response{
		Abi:     sys.ABIVersion,
		Code:    string(result.Errno()),
		Result:  result.Result(),
		Message: result.Message(),
		Labels:  result.Labels(),
	}
	switch result.Status() {
	case sys.StatusResult:
		response.Status = StatusResult
	case sys.StatusYield:
		response.Status = StatusYield
	case sys.StatusFailed:
		response.Status = StatusFailed
	}
	return response
}

// proto3 wire types used by the envelope.
const (
	wireVarint  = 0
	wireI64     = 1
	wireBytes   = 2
	wireI32     = 5
	maxVarintLn = 10
)

var errTruncated = errors.New("wire: truncated message")

// walkFields drives a decode: visit is called per field with either payload
// (wireBytes) or value (wireVarint) set. Unknown fields and wire types the
// envelope does not use are skipped — that is the schema-evolution contract.
func walkFields(data []byte, visit func(field, wireType uint64, payload []byte, value uint64) error) error {
	for len(data) > 0 {
		tag, n, err := consumeVarint(data)
		if err != nil {
			return err
		}
		data = data[n:]
		field, wireType := tag>>3, tag&7
		if field == 0 {
			return errors.New("wire: field number 0 is invalid")
		}

		var payload []byte
		var value uint64
		switch wireType {
		case wireVarint:
			value, n, err = consumeVarint(data)
			if err != nil {
				return err
			}
			data = data[n:]
		case wireBytes:
			length, n, err := consumeVarint(data)
			if err != nil {
				return err
			}
			data = data[n:]
			if uint64(len(data)) < length {
				return errTruncated
			}
			payload, data = data[:length], data[length:]
		case wireI64:
			if len(data) < 8 {
				return errTruncated
			}
			data = data[8:] // skippable: the envelope has no i64 fields
		case wireI32:
			if len(data) < 4 {
				return errTruncated
			}
			data = data[4:] // skippable: the envelope has no i32 fields
		default:
			return fmt.Errorf("wire: unsupported wire type %d", wireType)
		}
		if err := visit(field, wireType, payload, value); err != nil {
			return err
		}
	}
	return nil
}

func consumeVarint(data []byte) (uint64, int, error) {
	var value uint64
	for i := 0; i < len(data) && i < maxVarintLn; i++ {
		value |= uint64(data[i]&0x7F) << (7 * i)
		if data[i] < 0x80 {
			return value, i + 1, nil
		}
	}
	if len(data) == 0 || len(data) >= maxVarintLn {
		return 0, 0, errors.New("wire: malformed varint")
	}
	return 0, 0, errTruncated
}

// appendVarintField omits zero values, matching proto3 presence semantics.
func appendVarintField(b []byte, field uint64, value uint64) []byte {
	if value == 0 {
		return b
	}
	b = appendVarint(b, field<<3|wireVarint)
	return appendVarint(b, value)
}

// appendBytesField omits empty payloads, matching proto3 presence semantics.
func appendBytesField(b []byte, field uint64, payload []byte) []byte {
	if len(payload) == 0 {
		return b
	}
	b = appendVarint(b, field<<3|wireBytes)
	b = appendVarint(b, uint64(len(payload)))
	return append(b, payload...)
}

func appendVarint(b []byte, value uint64) []byte {
	for value >= 0x80 {
		b = append(b, byte(value)|0x80)
		value >>= 7
	}
	return append(b, byte(value))
}
