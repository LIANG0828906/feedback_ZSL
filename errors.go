package bonerig

import (
	"encoding/json"
	"fmt"
)

// Code is one of the stable acceptance error codes.
type Code string

const (
	CodeSchema Code = "E_SCHEMA"
	CodeQuat   Code = "E_QUAT"
	CodeMap    Code = "E_MAP"
	CodeLossy  Code = "E_LOSSY"
	CodeUB     Code = "E_UB"
	CodeState  Code = "E_STATE"
	CodeDiff   Code = "E_DIFF"
)

// Error carries a stable code and machine-readable location/operands.
type Error struct {
	Code       Code   `json:"code"`
	Message    string `json:"message"`
	Field      string `json:"field,omitempty"`
	Joint      string `json:"joint,omitempty"`
	Frame      *int   `json:"frame,omitempty"`
	IntervalID string `json:"interval_id,omitempty"`
	Breakpoint string `json:"breakpoint,omitempty"`
	State      string `json:"state,omitempty"`
	OpA        any    `json:"op_a,omitempty"`
	OpB        any    `json:"op_b,omitempty"`
	Expected   any    `json:"expected,omitempty"`
	Actual     any    `json:"actual,omitempty"`
}

func (e *Error) Error() string {
	b, _ := json.Marshal(e)
	return string(b)
}

func errorf(code Code, msg string) *Error {
	return &Error{Code: code, Message: msg}
}

func withFrame(err *Error, joint string, frame int) *Error {
	err.Joint = joint
	f := frame
	err.Frame = &f
	return err
}

// LossyItem describes one field that an older format cannot represent exactly.
type LossyItem struct {
	Code        Code   `json:"code"`
	FieldPath   string `json:"field_path"`
	Kind        string `json:"kind"`
	Disposition string `json:"disposition"`
	OldValue    any    `json:"old_value,omitempty"`
	Default     any    `json:"default,omitempty"`
	RuleVersion string `json:"rule_version"`
}

// TransformTrace is one auditable field conversion or default fill.
type TransformTrace struct {
	RuleVersion string `json:"rule_version"`
	Direction   string `json:"direction"`
	FieldPath   string `json:"field_path"`
	OldValue    any    `json:"old_value,omitempty"`
	NewValue    any    `json:"new_value,omitempty"`
	Default     any    `json:"default,omitempty"`
	Reason      string `json:"reason"`
}

// DiffDetail locates the first three-path mismatch.
type DiffDetail struct {
	IntervalID string `json:"interval_id,omitempty"`
	Joint      string `json:"joint"`
	Frame      int    `json:"frame"`
	Field      string `json:"field"`
	Expected   any    `json:"expected"`
	Actual     any    `json:"actual"`
}

// DiffError returns E_DIFF with the first location.
func DiffError(d DiffDetail) *Error {
	return &Error{
		Code:     CodeDiff,
		Message:  fmt.Sprintf("frame %d joint %s field %s differs", d.Frame, d.Joint, d.Field),
		Joint:    d.Joint,
		Frame:    &d.Frame,
		Field:    d.Field,
		Expected: d.Expected,
		Actual:   d.Actual,
	}
}
