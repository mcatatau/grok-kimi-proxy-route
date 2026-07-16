package signup

import (
	"encoding/binary"
	"fmt"
)

// Minimal protobuf wire helpers for the known AuthManagement messages.
// We only need string / embedded-message fields for the signup path.

func appendVarint(b []byte, v uint64) []byte {
	for v >= 0x80 {
		b = append(b, byte(v)|0x80)
		v >>= 7
	}
	return append(b, byte(v))
}

func appendKey(b []byte, fieldNumber int, wireType int) []byte {
	return appendVarint(b, uint64(fieldNumber<<3|wireType))
}

func AppendString(b []byte, fieldNumber int, s string) []byte {
	b = appendKey(b, fieldNumber, 2) // length-delimited
	b = appendVarint(b, uint64(len(s)))
	return append(b, s...)
}

func AppendBytes(b []byte, fieldNumber int, p []byte) []byte {
	b = appendKey(b, fieldNumber, 2)
	b = appendVarint(b, uint64(len(p)))
	return append(b, p...)
}

func AppendMessage(b []byte, fieldNumber int, msg []byte) []byte {
	return AppendBytes(b, fieldNumber, msg)
}

// GRPCWebFrame wraps a protobuf message in the gRPC-Web data frame:
// 1 byte flags (0) + 4 byte big-endian length + message.
func GRPCWebFrame(msg []byte) []byte {
	out := make([]byte, 5+len(msg))
	out[0] = 0 // data, not trailer
	binary.BigEndian.PutUint32(out[1:5], uint32(len(msg)))
	copy(out[5:], msg)
	return out
}

// CreateEmailValidationCodeRequest:
//
//	string email = 1;
//	string castle_request_token = 3;
func CreateEmailValidationCodeRequest(email, castleToken string) []byte {
	var b []byte
	b = AppendString(b, 1, email)
	if castleToken != "" {
		b = AppendString(b, 3, castleToken)
	}
	return b
}

// VerifyEmailValidationCodeRequest:
//
//	string email = 1;
//	string email_validation_code = 2;  // best-effort field numbers from client usage order
//
// Field numbers for verify are inferred from JS object keys order and common
// proto style; adjust if server rejects with decode errors.
func VerifyEmailValidationCodeRequest(email, code string) []byte {
	var b []byte
	b = AppendString(b, 1, email)
	b = AppendString(b, 2, code)
	return b
}

// CreateUserAndSessionRequest (inner):
//
//	string email = 1;
//	string given_name = 2;
//	string family_name = 3;
//	string clear_text_password = 4;
//	int32  tos_accepted_version = 5; // may be absent
//
// Outer wrapper used by the server-action path includes turnstile + castle.
// For direct AuthManagement.createUserAndSession the shape may differ —
// keep both builders.
func CreateUserAndSessionInner(email, given, family, password string, tosVersion int32) []byte {
	var b []byte
	b = AppendString(b, 1, email)
	if given != "" {
		b = AppendString(b, 2, given)
	}
	if family != "" {
		b = AppendString(b, 3, family)
	}
	if password != "" {
		b = AppendString(b, 4, password)
	}
	if tosVersion > 0 {
		b = appendKey(b, 5, 0)
		b = appendVarint(b, uint64(tosVersion))
	}
	return b
}

// ParseGRPCWebResponse strips the first data frame and returns message bytes.
// Returns trailer/status info if present in a second frame (best-effort).
func ParseGRPCWebResponse(body []byte) (msg []byte, grpcStatus string, err error) {
	if len(body) < 5 {
		return nil, "", fmt.Errorf("short grpc-web body: %d", len(body))
	}
	// may be multiple frames
	off := 0
	var data []byte
	for off+5 <= len(body) {
		flag := body[off]
		n := int(binary.BigEndian.Uint32(body[off+1 : off+5]))
		off += 5
		if off+n > len(body) {
			return data, grpcStatus, fmt.Errorf("truncated frame n=%d remain=%d", n, len(body)-off)
		}
		chunk := body[off : off+n]
		off += n
		if flag&0x80 != 0 {
			// trailer frame: ascii headers
			grpcStatus = string(chunk)
		} else if data == nil {
			data = chunk
		}
	}
	return data, grpcStatus, nil
}
