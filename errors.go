package maskqc

import (
	"errors"
	"fmt"
)

// ErrorCode is the stable machine-readable failure category used by the domain API.
type ErrorCode string

const (
	ESchema ErrorCode = "E_SCHEMA"
	ELossy  ErrorCode = "E_LOSSY"
	EOVF    ErrorCode = "E_OVF"
	EPara   ErrorCode = "E_PARA"
	ETop    ErrorCode = "E_TOP"
	EDiff   ErrorCode = "E_DIFF"

	// EBudget and ECorrupt are operational diagnostics layered on top of the
	// business codes requested by the specification.
	EBudget  ErrorCode = "E_BUDGET"
	ECorrupt ErrorCode = "E_CORRUPT"
)

// Error is the structured domain error. FieldPath uses dotted JSON-like paths.
type Error struct {
	Code      ErrorCode      `json:"code"`
	FieldPath string         `json:"field_path,omitempty"`
	Message   string         `json:"message"`
	Operands  [2]int64       `json:"operands,omitempty"`
	Details   map[string]any `json:"details,omitempty"`
}

func (e *Error) Error() string {
	if e.FieldPath != "" {
		return fmt.Sprintf("%s at %s: %s", e.Code, e.FieldPath, e.Message)
	}
	return fmt.Sprintf("%s: %s", e.Code, e.Message)
}

func newError(code ErrorCode, path, format string, args ...any) *Error {
	return &Error{Code: code, FieldPath: path, Message: fmt.Sprintf(format, args...)}
}

// NewSchemaError lets adapters outside the core package construct stable E_SCHEMA errors.
func NewSchemaError(path, format string, args ...any) *Error {
	return newError(ESchema, path, format, args...)
}

func ovfError(path string, a, b int64) *Error {
	return &Error{
		Code:      EOVF,
		FieldPath: path,
		Message:   "integer conversion overflow",
		Operands:  [2]int64{a, b},
	}
}

func paraError(path string, value any, min, max int) *Error {
	return &Error{
		Code:      EPara,
		FieldPath: path,
		Message:   fmt.Sprintf("value %v outside legal interval [%d,%d]", value, min, max),
		Details:   map[string]any{"value": value, "min": min, "max": max},
	}
}

// AsError unwraps a domain Error when possible.
func AsError(err error) (*Error, bool) {
	var de *Error
	if errors.As(err, &de) {
		return de, true
	}
	return nil, false
}
