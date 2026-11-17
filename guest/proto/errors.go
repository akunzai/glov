package proto

import (
	"errors"
	"fmt"
	"io/fs"
)

// Error codes from docs/guest-protocol.md.
const (
	CodeParse          = -32700
	CodeInvalidRequest = -32600
	CodeNoMethod       = -32601
	CodeInvalidParams  = -32602
	CodeOS             = 1
	CodeNotFound       = 2
	CodePermission     = 3
	CodeNoUser         = 4
)

// Error is a JSON-RPC error object.
type Error struct {
	Code    int    `json:"code"`
	Message string `json:"message"`
}

func (e *Error) Error() string { return fmt.Sprintf("rpc error %d: %s", e.Code, e.Message) }

// Errorf builds an Error with a formatted message.
func Errorf(code int, format string, args ...any) *Error {
	return &Error{Code: code, Message: fmt.Sprintf(format, args...)}
}

// FromOS maps an operating-system error to a protocol error (1, 2 or 3).
// An error that already is an *Error is returned unchanged.
func FromOS(err error) *Error {
	if pe, ok := errors.AsType[*Error](err); ok {
		return pe
	}
	switch {
	case errors.Is(err, fs.ErrNotExist):
		return &Error{CodeNotFound, err.Error()}
	case errors.Is(err, fs.ErrPermission):
		return &Error{CodePermission, err.Error()}
	}
	return &Error{CodeOS, err.Error()}
}
