// Package audioerr defines the stable error vocabulary used by the dialogue
// audio domain component.
package audioerr

import (
	"errors"
	"fmt"
	"strings"
)

// Code is a stable machine-readable error code.
type Code string

const (
	CodeSchema     Code = "E_SCHEMA"
	CodeLossy      Code = "E_LOSSY"
	CodeOverflow   Code = "E_OVF"
	CodeLoudness   Code = "E_LOUD"
	CodeState      Code = "E_STATE"
	CodeIO         Code = "E_IO"
	CodeCheckpoint Code = "E_CHECKPOINT"
	CodeCancelled  Code = "E_CANCELLED"
	CodeBudget     Code = "E_BUDGET"
	CodeMismatch   Code = "E_MISMATCH"
)

// Error carries a stable code, the offending field/path, operands for
// arithmetic failures, and optional structured values for diagnosis.
type Error struct {
	Code    Code           `json:"code"`
	Field   string         `json:"field,omitempty"`
	Message string         `json:"message"`
	OpA     int64          `json:"op_a,omitempty"`
	OpB     int64          `json:"op_b,omitempty"`
	Values  map[string]any `json:"values,omitempty"`
}

func (e *Error) Error() string {
	if e == nil {
		return ""
	}
	parts := []string{string(e.Code)}
	if e.Field != "" {
		parts = append(parts, e.Field)
	}
	if e.Message != "" {
		parts = append(parts, e.Message)
	}
	if e.OpA != 0 || e.OpB != 0 {
		parts = append(parts, fmt.Sprintf("operands=(%d,%d)", e.OpA, e.OpB))
	}
	return strings.Join(parts, ": ")
}

// New constructs a coded error.
func New(code Code, field, message string) *Error {
	return &Error{Code: code, Field: field, Message: message}
}

// WithValues attaches diagnostic values without changing the stable fields.
func (e *Error) WithValues(values map[string]any) *Error {
	if e.Values == nil {
		e.Values = make(map[string]any, len(values))
	}
	for key, value := range values {
		e.Values[key] = value
	}
	return e
}

// Schema reports a schema/path error.
func Schema(field, message string) *Error {
	return New(CodeSchema, field, message)
}

// Overflow reports a checked-integer failure and both operands.
func Overflow(field string, a, b int64) *Error {
	return &Error{
		Code:    CodeOverflow,
		Field:   field,
		Message: "integer overflow",
		OpA:     a,
		OpB:     b,
	}
}

// State reports an illegal state transition.
func State(field, message string) *Error {
	return New(CodeState, field, message)
}

// IsCode reports whether err is (or wraps) an Error with code.
func IsCode(err error, code Code) bool {
	var target *Error
	return errors.As(err, &target) && target.Code == code
}

// As returns the concrete coded error when available.
func As(err error) (*Error, bool) {
	var target *Error
	if errors.As(err, &target) {
		return target, true
	}
	return nil, false
}
