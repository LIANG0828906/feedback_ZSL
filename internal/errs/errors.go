package errs

import (
	"errors"
	"fmt"
)

type Code string

const (
	EDecode   Code = "E_DECODE"
	EPara     Code = "E_PARA"
	EScale    Code = "E_SCALE"
	EState    Code = "E_STATE"
	EDiff     Code = "E_DIFF"
	EVerify   Code = "E_VERIFY"
	EIO       Code = "E_IO"
	EBudget   Code = "E_BUDGET"
	ECancel   Code = "E_CANCEL"
	EInternal Code = "E_INTERNAL"
)

type Mismatch struct {
	Variant  string `json:"variant,omitempty"`
	Artifact string `json:"artifact,omitempty"`
	Field    string `json:"field"`
	Actual   any    `json:"actual"`
	Expected any    `json:"expected"`
}

type Error struct {
	Code     Code      `json:"code"`
	Message  string    `json:"message"`
	Field    string    `json:"field,omitempty"`
	Variant  string    `json:"variant,omitempty"`
	Detail   string    `json:"detail,omitempty"`
	Mismatch *Mismatch `json:"mismatch,omitempty"`
	Cause    error     `json:"-"`
}

func (e *Error) Error() string {
	if e == nil {
		return "<nil>"
	}
	msg := string(e.Code) + ": " + e.Message
	if e.Field != "" {
		msg += " (field=" + e.Field + ")"
	}
	if e.Variant != "" {
		msg += " (variant=" + e.Variant + ")"
	}
	if e.Detail != "" {
		msg += ": " + e.Detail
	}
	if e.Cause != nil {
		msg += ": " + e.Cause.Error()
	}
	return msg
}

func (e *Error) Unwrap() error { return e.Cause }

func New(code Code, message string) *Error {
	return &Error{Code: code, Message: message}
}

func Field(code Code, field, message string) *Error {
	return &Error{Code: code, Message: message, Field: field}
}

func Variant(code Code, variant, message string) *Error {
	return &Error{Code: code, Message: message, Variant: variant}
}

func Wrap(code Code, message string, cause error) *Error {
	return &Error{Code: code, Message: message, Cause: cause}
}

func Diff(m Mismatch, message string) *Error {
	return &Error{Code: EDiff, Message: message, Variant: m.Variant, Mismatch: &m}
}

func CodeOf(err error) Code {
	var app *Error
	if errors.As(err, &app) {
		return app.Code
	}
	return EInternal
}

func As(err error) *Error {
	var app *Error
	if errors.As(err, &app) {
		return app
	}
	return &Error{Code: EInternal, Message: "internal error", Cause: err}
}

func Errorf(code Code, format string, args ...any) *Error {
	return &Error{Code: code, Message: fmt.Sprintf(format, args...)}
}
