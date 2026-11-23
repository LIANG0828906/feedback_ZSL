package errs

import (
	"errors"
	"fmt"
)

// Code is the stable machine-readable error family used by the workbench.
type Code string

const (
	EDecode   Code = "E_DECODE"
	ERoom     Code = "E_ROOM"
	EConn     Code = "E_CONN"
	EPara     Code = "E_PARA"
	EState    Code = "E_STATE"
	ECheck    Code = "E_CHECKPOINT"
	EDiff     Code = "E_DIFF"
	EResource Code = "E_RESOURCE"
	EInput    Code = "E_INPUT"
	ECanceled Code = "E_CANCELED"
)

// Error carries enough context for the CLI, HTTP API and tests to identify the
// exact room/listener/field that caused a failure.
type Error struct {
	Code     Code   `json:"code"`
	Room     string `json:"room,omitempty"`
	Source   string `json:"source,omitempty"`
	Listener string `json:"listener,omitempty"`
	Field    string `json:"field,omitempty"`
	Reason   string `json:"reason,omitempty"`
	Message  string `json:"message"`
	Wrapped  error  `json:"-"`
}

func (e *Error) Error() string {
	if e == nil {
		return ""
	}
	msg := string(e.Code)
	if e.Room != "" {
		msg += " room=" + e.Room
	}
	if e.Source != "" {
		msg += " source=" + e.Source
	}
	if e.Listener != "" {
		msg += " listener=" + e.Listener
	}
	if e.Field != "" {
		msg += " field=" + e.Field
	}
	if e.Message != "" {
		msg += ": " + e.Message
	}
	return msg
}

func (e *Error) Unwrap() error { return e.Wrapped }

func New(code Code, message string) *Error {
	return &Error{Code: code, Message: message}
}

func With(code Code, room, field, message string) *Error {
	return &Error{Code: code, Room: room, Field: field, Message: message}
}

func Wrap(code Code, err error) error {
	if err == nil {
		return nil
	}
	var existing *Error
	if errors.As(err, &existing) {
		return existing
	}
	return &Error{Code: code, Message: err.Error(), Wrapped: err}
}

func CodeOf(err error) Code {
	if err == nil {
		return ""
	}
	var typed *Error
	if errors.As(err, &typed) {
		return typed.Code
	}
	return ""
}

func IsCode(err error, code Code) bool {
	return CodeOf(err) == code
}

func (e *Error) Clone() *Error {
	if e == nil {
		return nil
	}
	c := *e
	return &c
}

func Decode(room, field string, format string, args ...any) *Error {
	return &Error{Code: EDecode, Room: room, Field: field, Message: fmt.Sprintf(format, args...)}
}
