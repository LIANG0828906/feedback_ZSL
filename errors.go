package maskqc

import (
	"errors"
	"fmt"
)

// Code is a stable machine-readable diagnostic code.
type Code string

const (
	CodeSchema Code = "E_SCHEMA"
	CodeLossy  Code = "E_LOSSY"
	CodeOvf    Code = "E_OVF"
	CodePara   Code = "E_PARA"
	CodeTop    Code = "E_TOP"
	CodeDiff   Code = "E_DIFF"
	CodeState  Code = "E_STATE"
)

// Error is the structured error type used by both domain and HTTP layers.
type Error struct {
	Code          Code           `json:"code"`
	Message       string         `json:"message"`
	Field         string         `json:"field,omitempty"`
	Path          string         `json:"path,omitempty"`
	SchemaVersion string         `json:"schema_version,omitempty"`
	Operands      []int64        `json:"operands,omitempty"`
	LegalRange    [2]int         `json:"legal_range,omitempty"`
	Details       map[string]any `json:"details,omitempty"`
}

func (e *Error) Error() string {
	if e == nil {
		return ""
	}
	if e.Path != "" {
		return fmt.Sprintf("%s %s: %s", e.Code, e.Path, e.Message)
	}
	return fmt.Sprintf("%s: %s", e.Code, e.Message)
}

func (e *Error) Unwrap() error { return nil }

// AsError returns the first *Error in err's chain.
func AsError(err error) (*Error, bool) {
	var target *Error
	if errors.As(err, &target) {
		return target, true
	}
	return nil, false
}

func schemaError(version, path, field, msg string) *Error {
	return &Error{Code: CodeSchema, Message: msg, Path: path, Field: field, SchemaVersion: version}
}

func paramError(field string, min, max int, msg string) *Error {
	return &Error{Code: CodePara, Message: msg, Field: field, LegalRange: [2]int{min, max}}
}

func overflowError(field string, a, b int64) *Error {
	return &Error{
		Code:     CodeOvf,
		Message:  "integer conversion overflow",
		Field:    field,
		Operands: []int64{a, b},
	}
}
