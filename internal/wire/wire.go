package wire

import (
	"bytes"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"io"
)

// SuperDB hosted wire protocol (v1).
//
// Frame layout, all integers big-endian:
//
//	0..3   magic "SDB1" (0x53 0x44 0x42 0x31)
//	4..5   protocol version uint16 (currently 1)
//	6      message type uint8
//	7      flags uint8 (reserved, must be 0)
//	8..15  request ID uint64
//	16..19 payload length uint32 (bytes of JSON payload that follows)
//	20..   JSON payload
//
// The payload is JSON so drivers can be written in any language without a
// Go-specific decoder. Maximum frame payload is MaxFrameSize (16 MiB).
// Servers MUST validate magic, version, and length before allocating.
var Magic = [4]byte{'S', 'D', 'B', '1'}

// Version is the current protocol version.
const Version uint16 = 1

// HeaderLen is the fixed frame header size.
const HeaderLen = 20

// MaxFrameSize bounds any single payload allocation.
const MaxFrameSize = 16 << 20

// MaxQuerySize bounds SQL text inside a request payload.
const MaxQuerySize = 4 << 20

// Message types.
const (
	TypeResponse uint8 = 0x80
	TypeHello    uint8 = 1
	TypeAuth     uint8 = 2
	TypePing     uint8 = 3
	TypeQuery    uint8 = 4
	TypeExec     uint8 = 5
	TypeBegin    uint8 = 6
	TypeCommit   uint8 = 7
	TypeRollback uint8 = 8
	TypeClose    uint8 = 9
)

// Stable error codes returned in Response.ErrorCode.
const (
	ErrAuthFailed     = "AUTH_FAILED"
	ErrAuthRequired   = "AUTH_REQUIRED"
	ErrInvalidRequest = "INVALID_REQUEST"
	ErrQueryError     = "QUERY_ERROR"
	ErrNotFound       = "NOT_FOUND"
	ErrNotLeader      = "NOT_LEADER"
	ErrTimeout        = "TIMEOUT"
	ErrUnsupported    = "UNSUPPORTED"
	ErrInternal       = "INTERNAL_ERROR"
	ErrProtocol       = "PROTOCOL_ERROR"
	// ErrBusy marks retryable admission rejections (MaxInflightQueries).
	ErrBusy = "SERVER_BUSY"
	// ErrResultTooLarge marks MaxResultRows/MaxResultBytes violations.
	ErrResultTooLarge = "RESULT_TOO_LARGE"
)

// Request payloads (JSON).

type HelloPayload struct {
	ClientName    string `json:"client_name,omitempty"`
	ClientVersion string `json:"client_version,omitempty"`
}

type AuthPayload struct {
	Mechanism string `json:"mechanism,omitempty"` // "password" (default) or "token"
	Username  string `json:"username,omitempty"`
	Password  string `json:"password,omitempty"`
	Token     string `json:"token,omitempty"`
	Database  string `json:"database,omitempty"`
}

type QueryPayload struct {
	SQL      string `json:"sql"`
	Params   []any  `json:"params,omitempty"`
	Database string `json:"database,omitempty"`
}

type ExecPayload struct {
	SQL      string `json:"sql"`
	Params   []any  `json:"params,omitempty"`
	Database string `json:"database,omitempty"`
}

// Response is the single response envelope for every operation.
type Response struct {
	RequestID    uint64   `json:"request_id"`
	Status       string   `json:"status"` // "ok" or "error"
	ErrorCode    string   `json:"error_code,omitempty"`
	ErrorMessage string   `json:"error_message,omitempty"`
	Columns      []string `json:"columns,omitempty"`
	Rows         [][]any  `json:"rows,omitempty"`
	AffectedRows int      `json:"affected_rows,omitempty"`
	Message      string   `json:"message,omitempty"`
	LeaderAddr   string   `json:"leader_addr,omitempty"`
	Retryable    bool     `json:"retryable,omitempty"`
}

// Error implements error.
func (r Response) Error() string {
	if r.Status == "ok" {
		return ""
	}
	if r.ErrorCode != "" {
		return r.ErrorCode + ": " + r.ErrorMessage
	}
	return r.ErrorMessage
}

// Header is a decoded frame header.
type Header struct {
	Version   uint16
	Type      uint8
	Flags     uint8
	RequestID uint64
	Length    uint32
}

// EncodeFrame builds a frame into buf.
func EncodeFrame(msgType uint8, requestID uint64, payload []byte) []byte {
	out := make([]byte, 0, HeaderLen+len(payload))
	out = append(out, Magic[:]...)
	var tmp [16]byte
	binary.BigEndian.PutUint16(tmp[0:2], Version)
	tmp[2] = msgType
	tmp[3] = 0
	binary.BigEndian.PutUint64(tmp[4:12], requestID)
	binary.BigEndian.PutUint32(tmp[12:16], uint32(len(payload)))
	out = append(out, tmp[:]...)
	out = append(out, payload...)
	return out
}

// WriteFrame writes one frame.
func WriteFrame(w io.Writer, msgType uint8, requestID uint64, payload []byte) error {
	if len(payload) > MaxFrameSize {
		return fmt.Errorf("payload too large: %d", len(payload))
	}
	frame := EncodeFrame(msgType, requestID, payload)
	_, err := w.Write(frame)
	return err
}

// MustJSON marshals v to JSON.
func MustJSON(v any) []byte {
	b, err := json.Marshal(v)
	if err != nil {
		panic(err)
	}
	return b
}

// ReadHeader reads and validates a frame header.
func ReadHeader(r io.Reader) (Header, error) {
	var h [HeaderLen]byte
	if _, err := io.ReadFull(r, h[:]); err != nil {
		return Header{}, err
	}
	if !bytes.Equal(h[0:4], Magic[:]) {
		return Header{}, errors.New("bad magic: expected SDB1")
	}
	ver := binary.BigEndian.Uint16(h[4:6])
	if ver != Version {
		return Header{}, &VersionError{Version: ver}
	}
	if h[7] != 0 {
		return Header{}, errors.New("unsupported flags")
	}
	length := binary.BigEndian.Uint32(h[16:20])
	if length > MaxFrameSize {
		return Header{}, fmt.Errorf("frame too large: %d bytes", length)
	}
	return Header{
		Version:   ver,
		Type:      h[6],
		Flags:     h[7],
		RequestID: binary.BigEndian.Uint64(h[8:16]),
		Length:    length,
	}, nil
}

// VersionError is returned for unknown protocol versions.
type VersionError struct{ Version uint16 }

func (e *VersionError) Error() string {
	return fmt.Sprintf("unsupported protocol version %d (server speaks %d)", e.Version, Version)
}

// ReadFrame reads one full frame: header + bounded payload.
func ReadFrame(r io.Reader) (Header, []byte, error) {
	h, err := ReadHeader(r)
	if err != nil {
		return Header{}, nil, err
	}
	payload := make([]byte, h.Length)
	if _, err := io.ReadFull(r, payload); err != nil {
		return Header{}, nil, err
	}
	return h, payload, nil
}

// ErrorResponse builds an error Response payload.
func ErrorResponse(requestID uint64, code, msg string) []byte {
	return MustJSON(Response{RequestID: requestID, Status: "error", ErrorCode: code, ErrorMessage: msg})
}

// OKResponse builds a success Response payload.
func OKResponse(requestID uint64) []byte {
	return MustJSON(Response{RequestID: requestID, Status: "ok"})
}
