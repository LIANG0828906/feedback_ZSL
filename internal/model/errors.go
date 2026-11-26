package model

import (
	"errors"
	"fmt"
)

type Code string

const (
	ESchema   Code = "E_SCHEMA"
	ELossy    Code = "E_LOSSY"
	EOvf      Code = "E_OVF"
	EPara     Code = "E_PARA"
	EConflict Code = "E_CONFLICT"
	EState    Code = "E_STATE"
	EDecode   Code = "E_DECODE"
	EOrder    Code = "E_ORDER"
	ECap      Code = "E_CAP"
	EBuffer   Code = "E_BUFFER"
	EInternal Code = "E_INTERNAL"
)

type Error struct {
	Code     Code           `json:"code"`
	Message  string         `json:"message"`
	Path     string         `json:"path,omitempty"`
	Field    string         `json:"field,omitempty"`
	ScreenID string         `json:"screen_id,omitempty"`
	OpA      string         `json:"op_a,omitempty"`
	OpB      string         `json:"op_b,omitempty"`
	Actual   string         `json:"actual,omitempty"`
	Details  map[string]any `json:"details,omitempty"`
	Cause    error          `json:"-"`
}

func (e *Error) Error() string {
	msg := string(e.Code)
	if e.Path != "" {
		msg += " path=" + e.Path
	}
	if e.Field != "" && e.Path == "" {
		msg += " field=" + e.Field
	}
	if e.ScreenID != "" {
		msg += " screen=" + e.ScreenID
	}
	if e.OpA != "" || e.OpB != "" {
		msg += " operands=(" + e.OpA + "," + e.OpB + ")"
	}
	if e.Message != "" {
		msg += ": " + e.Message
	}
	if e.Cause != nil {
		msg += ": " + e.Cause.Error()
	}
	return msg
}

func (e *Error) Unwrap() error { return e.Cause }

func NewError(code Code, format string, args ...any) *Error {
	return &Error{Code: code, Message: fmt.Sprintf(format, args...)}
}

func SchemaError(path, format string, args ...any) *Error {
	return &Error{Code: ESchema, Path: path, Message: fmt.Sprintf(format, args...)}
}

func OvfError(field string, a, b any) *Error {
	return &Error{
		Code:    EOvf,
		Field:   field,
		Path:    "$." + field,
		Message: "integer operation overflow",
		OpA:     fmt.Sprint(a),
		OpB:     fmt.Sprint(b),
	}
}

func AsError(err error) (*Error, bool) {
	var target *Error
	if errors.As(err, &target) {
		return target, true
	}
	return nil, false
}

func HasCode(err error, code Code) bool {
	var target *Error
	if errors.As(err, &target) {
		return target.Code == code
	}
	return false
}
