package cluster

import "fmt"

// Code identifies a cluster error category.
type Code string

const (
	CodeNotFound         Code = "not_found"
	CodeUnavailable      Code = "unavailable"
	CodeTimeout          Code = "timeout"
	CodeVersionMismatch  Code = "version_mismatch"
	CodeCorruptMetadata  Code = "corrupt_metadata"
	CodeClusterMismatch  Code = "cluster_mismatch"
	CodeNotImplemented   Code = "not_implemented"
	CodeNoQuorum         Code = "no_quorum"
	CodeNoLeader         Code = "no_leader"
	CodeShutdown         Code = "shutdown"
	CodeInvalidArgument  Code = "invalid_argument"
	CodePersist          Code = "persist"
	CodeTransport        Code = "transport"
	CodeRangeNotFound    Code = "range_not_found"
	CodeDurabilityUnsure Code = "durability_unsure"
)

// Error is a structured cluster error.
type Error struct {
	Code      Code
	Message   string
	NodeID    string
	Retryable bool
}

func (e *Error) Error() string {
	if e.NodeID != "" {
		return fmt.Sprintf("cluster %s (node %s): %s", e.Code, e.NodeID, e.Message)
	}
	return fmt.Sprintf("cluster %s: %s", e.Code, e.Message)
}

// NewError builds a structured cluster error.
func NewError(code Code, msg string) *Error { return &Error{Code: code, Message: msg} }

// WithNode attaches node context.
func (e *Error) WithNode(id string) *Error {
	c := *e
	c.NodeID = id
	return &c
}

// AsError converts err to *Error when possible.
func AsError(err error) (*Error, bool) {
	e, ok := err.(*Error)
	return e, ok
}
