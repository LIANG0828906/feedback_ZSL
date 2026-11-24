package rverr

import (
	"errors"
	"fmt"
)

type Code string

const (
	EDecode Code = "E_DECODE"
	EMeta   Code = "E_META"
	EOrder  Code = "E_ORDER"
	EPara   Code = "E_PARA"
	ERange  Code = "E_RANGE"
	EDiff   Code = "E_DIFF"
	EStore  Code = "E_STORE"
	EFault  Code = "E_FAULT"
)

type Error struct {
	Code    Code   `json:"code"`
	Field   string `json:"field,omitempty"`
	Frame   int    `json:"frame,omitempty"`
	BlockX  int    `json:"block_x,omitempty"`
	BlockY  int    `json:"block_y,omitempty"`
	Message string `json:"message"`
}

func (e *Error) Error() string {
	switch {
	case e.Frame > 0 && e.Field != "":
		return fmt.Sprintf("%s frame=%d field=%s: %s", e.Code, e.Frame, e.Field, e.Message)
	case e.Frame > 0:
		return fmt.Sprintf("%s frame=%d: %s", e.Code, e.Frame, e.Message)
	case e.Field != "":
		return fmt.Sprintf("%s field=%s: %s", e.Code, e.Field, e.Message)
	default:
		return fmt.Sprintf("%s: %s", e.Code, e.Message)
	}
}

func New(code Code, msg string) *Error {
	return &Error{Code: code, Message: msg}
}

func Field(code Code, field, msg string) *Error {
	return &Error{Code: code, Field: field, Message: msg}
}

func Frame(code Code, frame int, field, msg string) *Error {
	return &Error{Code: code, Frame: frame, Field: field, Message: msg}
}

func Wrap(code Code, msg string, err error) error {
	if err == nil {
		return nil
	}
	return &Error{Code: code, Message: msg + ": " + err.Error()}
}

func As(err error) *Error {
	var target *Error
	if errors.As(err, &target) {
		return target
	}
	return nil
}

func CodeOf(err error) Code {
	if e := As(err); e != nil {
		return e.Code
	}
	return ""
}
