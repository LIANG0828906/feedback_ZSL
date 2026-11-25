package ocrflow

import (
	"errors"
	"fmt"
)

// Code is a stable machine-readable error category.
type Code string

const (
	CodeSchema   Code = "E_SCHEMA"
	CodeOverflow Code = "E_OVF"
	CodeParam    Code = "E_PARA"
	CodeDecode   Code = "E_DECODE"
	CodeDiff     Code = "E_DIFF"
	CodeResource Code = "E_RESOURCE"
	CodeConflict Code = "E_CONFLICT"
)

// Error is the structured error type used by both Go callers and the local API.
type Error struct {
	Code          Code           `json:"code"`
	Message       string         `json:"message"`
	Field         string         `json:"field,omitempty"`
	SourceVersion string         `json:"source_version,omitempty"`
	OperandA      string         `json:"operand_a,omitempty"`
	OperandB      string         `json:"operand_b,omitempty"`
	AllowedSet    any            `json:"allowed_set,omitempty"`
	ShotID        string         `json:"shot_id,omitempty"`
	LineNo        int            `json:"line_no,omitempty"`
	DiffField     string         `json:"diff_field,omitempty"`
	Expected      any            `json:"expected,omitempty"`
	Actual        any            `json:"actual,omitempty"`
	Meta          map[string]any `json:"meta,omitempty"`
	Cause         error          `json:"-"`
}

func (e *Error) Error() string {
	if e.Field != "" {
		return fmt.Sprintf("%s %s: %s", e.Code, e.Field, e.Message)
	}
	return fmt.Sprintf("%s: %s", e.Code, e.Message)
}

func (e *Error) Unwrap() error { return e.Cause }

func asError(err error) (*Error, bool) {
	if err == nil {
		return nil, false
	}
	var oe *Error
	if errors.As(err, &oe) {
		return oe, true
	}
	return nil, false
}

// ErrorCode returns the stable structured code for an error, or empty string.
func ErrorCode(err error) Code {
	if e, ok := asError(err); ok {
		return e.Code
	}
	return ""
}

func schemaError(version, field, format string, args ...any) error {
	return &Error{Code: CodeSchema, SourceVersion: version, Field: field, Message: fmt.Sprintf(format, args...)}
}

func paramError(field string, allowed any, format string, args ...any) error {
	return &Error{Code: CodeParam, Field: field, AllowedSet: allowed, Message: fmt.Sprintf(format, args...)}
}

func overflowError(field string, a, b any) error {
	return &Error{
		Code:     CodeOverflow,
		Field:    field,
		OperandA: fmt.Sprintf("%v", a),
		OperandB: fmt.Sprintf("%v", b),
		Message:  "integer conversion overflow",
	}
}

func decodeError(field string, cause error, format string, args ...any) error {
	return &Error{Code: CodeDecode, Field: field, Message: fmt.Sprintf(format, args...), Cause: cause}
}

func diffError(shotID string, lineNo int, field string, expected, actual any) error {
	return &Error{
		Code:      CodeDiff,
		ShotID:    shotID,
		LineNo:    lineNo,
		DiffField: field,
		Expected:  expected,
		Actual:    actual,
		Message:   fmt.Sprintf("incremental/full mismatch at %s line %d field %s", shotID, lineNo, field),
	}
}
