package apperr

import (
	"errors"
	"fmt"
)

const (
	CodeMeta      = "E_META"
	CodePlane     = "E_PLANE"
	CodePara      = "E_PARA"
	CodeDiff      = "E_DIFF"
	CodeCache     = "E_CACHE"
	CodeCanceled  = "E_CANCELLED"
	CodeIO        = "E_IO"
	CodeResource  = "E_RESOURCE"
	CodeInvariant = "E_INVARIANT"
)

// Error is the stable, machine-readable error contract used by the CLI and tests.
type Error struct {
	Code     string `json:"code"`
	Message  string `json:"message"`
	Field    string `json:"field,omitempty"`
	Plane    string `json:"plane,omitempty"`
	Stage    string `json:"stage,omitempty"`
	Expected string `json:"expected,omitempty"`
	Observed string `json:"observed,omitempty"`
	X        *int   `json:"x,omitempty"`
	Y        *int   `json:"y,omitempty"`
	Cause    error  `json:"-"`
}

func (e *Error) Error() string {
	msg := e.Code + ": " + e.Message
	if e.Field != "" {
		msg += " (field=" + e.Field + ")"
	}
	if e.Plane != "" {
		msg += " (plane=" + e.Plane + ")"
	}
	if e.Stage != "" {
		msg += " (stage=" + e.Stage + ")"
	}
	if e.Expected != "" || e.Observed != "" {
		msg += fmt.Sprintf(" (expected=%s observed=%s)", e.Expected, e.Observed)
	}
	if e.X != nil && e.Y != nil {
		msg += fmt.Sprintf(" (x=%d,y=%d)", *e.X, *e.Y)
	}
	if e.Cause != nil {
		msg += ": " + e.Cause.Error()
	}
	return msg
}

func (e *Error) Unwrap() error { return e.Cause }

func New(code, message string) *Error {
	return &Error{Code: code, Message: message}
}

func Meta(field, message string) *Error {
	return &Error{Code: CodeMeta, Field: field, Message: message}
}

func Plane(name, expected, observed, reason string) *Error {
	return &Error{
		Code: CodePlane, Plane: name, Expected: expected, Observed: observed,
		Message: reason,
	}
}

func Para(field, message string) *Error {
	return &Error{Code: CodePara, Field: field, Message: message}
}

func Diff(stage string, x, y int, message string) *Error {
	return &Error{Code: CodeDiff, Stage: stage, X: &x, Y: &y, Message: message}
}

func Canceled(cause error) *Error {
	return &Error{Code: CodeCanceled, Message: "operation canceled at a persisted stage boundary", Cause: cause}
}

func IO(operation string, cause error) *Error {
	return &Error{Code: CodeIO, Message: operation, Cause: cause}
}

func Resource(message string) *Error {
	return &Error{Code: CodeResource, Message: message}
}

func Code(err error) string {
	var appErr *Error
	if errors.As(err, &appErr) {
		return appErr.Code
	}
	return ""
}
