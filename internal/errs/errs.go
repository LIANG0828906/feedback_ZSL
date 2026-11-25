package errs

import (
	"errors"
	"fmt"
)

type Code string

const (
	ESchema Code = "E_SCHEMA"
	EMeta   Code = "E_META"
	EPara   Code = "E_PARA"
	EState  Code = "E_STATE"
	EAttr   Code = "E_ATTR"
	ECache  Code = "E_CACHE"
	EIO     Code = "E_IO"
	EBuild  Code = "E_BUILD"
)

type Error struct {
	Code       Code
	Message    string
	StepIndex  int
	Field      string
	BlockX     int
	BlockY     int
	Candidates []int
	Err        error
}

func (e *Error) Error() string {
	where := ""
	if e.StepIndex > 0 {
		where = fmt.Sprintf(" step=%d", e.StepIndex)
	}
	if e.Field != "" {
		where += " field=" + e.Field
	}
	if e.BlockX >= 0 && e.BlockY >= 0 {
		where += fmt.Sprintf(" block=(%d,%d)", e.BlockX, e.BlockY)
	}
	msg := string(e.Code) + where + ": " + e.Message
	if e.Err != nil {
		msg += ": " + e.Err.Error()
	}
	return msg
}

func (e *Error) Unwrap() error { return e.Err }

func New(code Code, format string, args ...any) *Error {
	return &Error{Code: code, Message: fmt.Sprintf(format, args...), StepIndex: -1, BlockX: -1, BlockY: -1}
}

func Wrap(code Code, err error, format string, args ...any) *Error {
	return &Error{Code: code, Message: fmt.Sprintf(format, args...), StepIndex: -1, BlockX: -1, BlockY: -1, Err: err}
}

func WithStep(err error, step int, field string) error {
	var ke *Error
	if errors.As(err, &ke) {
		ke.StepIndex = step
		ke.Field = field
		return ke
	}
	return &Error{Code: EPara, Message: err.Error(), StepIndex: step, Field: field, BlockX: -1, BlockY: -1, Err: err}
}

func WithBlock(err error, x, y int, candidates []int) error {
	var ke *Error
	if errors.As(err, &ke) {
		ke.BlockX = x
		ke.BlockY = y
		if len(candidates) > 0 {
			ke.Candidates = append([]int(nil), candidates...)
		}
		return ke
	}
	return &Error{Code: EAttr, Message: err.Error(), BlockX: x, BlockY: y, Candidates: candidates, StepIndex: -1}
}

func CodeOf(err error) Code {
	var ke *Error
	if errors.As(err, &ke) {
		return ke.Code
	}
	return ""
}

func IsCode(err error, code Code) bool {
	return CodeOf(err) == code
}
