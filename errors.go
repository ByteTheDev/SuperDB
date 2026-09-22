package superdb

import "fmt"

// ServerError is a structured server failure. Applications should switch
// on Code instead of parsing Message.
type ServerError struct {
	Code       string
	Message    string
	Retryable  bool
	LeaderAddr string
}

func (e *ServerError) Error() string {
	if e.Code != "" {
		return fmt.Sprintf("%s: %s", e.Code, e.Message)
	}
	return e.Message
}

// Common error codes (mirrors the wire protocol).
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
	ErrBusy           = "SERVER_BUSY"
	ErrResultTooLarge = "RESULT_TOO_LARGE"
)
