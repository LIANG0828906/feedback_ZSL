package apperr

import (
	"errors"
	"fmt"
	"net/http"
)

const (
	Decode = "E_DECODE"
	Meta   = "E_META"
	Para   = "E_PARA"
	Scale  = "E_SCALE"
	State  = "E_STATE"
	Diff   = "E_DIFF"
	Order  = "E_ORDER"
	Mark   = "E_MARK"
	Frame  = "E_FRAME"
)

// Block identifies the smallest content block involved in an error.
type Block struct {
	Col int `json:"col"`
	Row int `json:"row"`
	X   int `json:"x"`
	Y   int `json:"y"`
	W   int `json:"w"`
	H   int `json:"h"`
}

// Error is the stable machine-readable error envelope used by the CLI and HTTP API.
type Error struct {
	Code           string         `json:"code"`
	Message        string         `json:"message"`
	Field          string         `json:"field,omitempty"`
	Frame          int            `json:"frame"`
	Block          Block          `json:"block"`
	Expected       any            `json:"expected,omitempty"`
	Actual         any            `json:"actual,omitempty"`
	AvailableRange string         `json:"available_range,omitempty"`
	Left           any            `json:"left,omitempty"`
	Right          any            `json:"right,omitempty"`
	Details        map[string]any `json:"details,omitempty"`
	Cause          error          `json:"-"`
}

func (e *Error) Error() string {
	if e == nil {
		return ""
	}
	msg := e.Message
	if msg == "" {
		msg = e.Code
	}
	if e.Field != "" {
		msg += " field=" + e.Field
	}
	if e.Frame >= 0 {
		msg += fmt.Sprintf(" frame=%d", e.Frame)
	}
	if e.Cause != nil {
		msg += ": " + e.Cause.Error()
	}
	return msg
}

func (e *Error) Unwrap() error { return e.Cause }

func New(code, message string) *Error {
	return &Error{Code: code, Message: message, Frame: -1, Block: Block{}}
}

func Wrap(code, message string, cause error) *Error {
	return &Error{Code: code, Message: message, Frame: -1, Block: Block{}, Cause: cause}
}

func WithField(err *Error, field string, expected, actual any, available string) *Error {
	if err == nil {
		return nil
	}
	err.Field = field
	err.Expected = expected
	err.Actual = actual
	err.AvailableRange = available
	return err
}

func At(err *Error, frame int, block Block) *Error {
	if err == nil {
		return nil
	}
	err.Frame = frame
	err.Block = block
	return err
}

func As(err error) *Error {
	var target *Error
	if errors.As(err, &target) {
		return target
	}
	return nil
}

func CodeOf(err error) string {
	if e := As(err); e != nil {
		return e.Code
	}
	return ""
}

func HTTPStatus(err error) int {
	e := As(err)
	if e == nil {
		return http.StatusInternalServerError
	}
	switch e.Code {
	case Decode, Meta, Para, Order, Mark, Frame:
		return http.StatusUnprocessableEntity
	case Scale, Diff, State:
		return http.StatusConflict
	default:
		return http.StatusInternalServerError
	}
}
