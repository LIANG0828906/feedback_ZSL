// Package errs defines the business error codes shared by every entry point.
// Error codes are emitted as a JSON "code" field on stderr and mapped to
// deterministic non-zero integer process exit codes so acceptance scripts can
// collect them on both Windows and POSIX.
package errs

import (
	"fmt"
)

const (
	ESchema  = "E_SCHEMA"
	EDecode  = "E_DECODE"
	EPara    = "E_PARA"
	EState   = "E_STATE"
	EBand    = "E_BAND"
	EBound   = "E_BOUND"
	EDiverge = "E_DIVERGE"
)

// ExitCode maps every business error code to a stable integer exit code.
func ExitCode(code string) int {
	switch code {
	case ESchema:
		return 10
	case EDecode:
		return 11
	case EPara:
		return 12
	case EState:
		return 13
	case EBand:
		return 14
	case EBound:
		return 15
	case EDiverge:
		return 16
	default:
		return 1
	}
}

// Error is a typed business error carrying a stable code and context fields.
type Error struct {
	Code    string            `json:"code"`
	Message string            `json:"message"`
	Fields  map[string]string `json:"fields,omitempty"`
}

func (e *Error) Error() string {
	if len(e.Fields) == 0 {
		return fmt.Sprintf("%s: %s", e.Code, e.Message)
	}
	return fmt.Sprintf("%s: %s (%v)", e.Code, e.Message, e.Fields)
}

// New builds an Error with the given code and message.
func New(code, message string) *Error {
	return &Error{Code: code, Message: message, Fields: map[string]string{}}
}

// With attaches a context field (for example the offending schema field name,
// frame number or block coordinate) and returns the same error.
func (e *Error) With(key, value string) *Error {
	if e.Fields == nil {
		e.Fields = map[string]string{}
	}
	e.Fields[key] = value
	return e
}

// WithInt attaches an integer context field.
func (e *Error) WithInt(key string, value int64) *Error {
	return e.With(key, fmt.Sprintf("%d", value))
}

// CodeOf extracts the business code from an error, returning "" for plain errors.
func CodeOf(err error) string {
	if be, ok := err.(*Error); ok {
		return be.Code
	}
	return ""
}
