package vpf

import (
	"errors"
	"fmt"
)

// ErrorCode is one of the stable machine-readable diagnostics used by VPF.
type ErrorCode string

const (
	CodeSchema ErrorCode = "E_SCHEMA"
	CodeLossy  ErrorCode = "E_LOSSY"
	CodeOVF    ErrorCode = "E_OVF"
	CodeTree   ErrorCode = "E_TREE"
	CodeChunk  ErrorCode = "E_CHUNK"
	CodeBudget ErrorCode = "E_BUDGET"
)

// Error is the structured error type returned by deterministic domain rules.
type Error struct {
	Code      ErrorCode `json:"code"`
	Message   string    `json:"message"`
	FieldPath string    `json:"field_path,omitempty"`
	Version   int       `json:"source_version,omitempty"`
	Left      any       `json:"operand_a,omitempty"`
	Right     any       `json:"operand_b,omitempty"`
	Details   any       `json:"details,omitempty"`
}

func (e *Error) Error() string {
	if e.FieldPath != "" {
		return fmt.Sprintf("%s %s (field=%s): %s", e.Code, e.Message, e.FieldPath, e.Details)
	}
	return fmt.Sprintf("%s %s: %v", e.Code, e.Message, e.Details)
}

func schemaError(version int, path, format string, args ...any) *Error {
	return &Error{
		Code:      CodeSchema,
		Message:   "schema validation failed",
		FieldPath: path,
		Version:   version,
		Details:   fmt.Sprintf(format, args...),
	}
}

func overflowError(field string, a, b any) *Error {
	return &Error{
		Code:      CodeOVF,
		Message:   "integer conversion overflow",
		FieldPath: field,
		Left:      a,
		Right:     b,
		Details:   "checked integer operation exceeded int64 range",
	}
}

// AsError unwraps err to *Error when possible.
func AsError(err error) (*Error, bool) {
	var target *Error
	if errors.As(err, &target) {
		return target, true
	}
	return nil, false
}

// HasCode reports whether err contains a VPF error with code.
func HasCode(err error, code ErrorCode) bool {
	var target *Error
	if errors.As(err, &target) {
		return target.Code == code
	}
	return false
}
