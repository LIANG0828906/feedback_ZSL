package model

import (
	"encoding/json"
	"errors"
	"fmt"
)

type ErrorCode string

const (
	E_SCHEMA ErrorCode = "E_SCHEMA"
	E_CYCLE  ErrorCode = "E_CYCLE"
	E_FRAME  ErrorCode = "E_FRAME"
	E_STATE  ErrorCode = "E_STATE"
	E_DIFF   ErrorCode = "E_DIFF"
	E_IO     ErrorCode = "E_IO"
	E_PARAM  ErrorCode = "E_PARAM"
)

// CodedError is the stable error shape returned across the library and HTTP API.
type CodedError struct {
	Code     ErrorCode `json:"code"`
	Message  string    `json:"message"`
	Joint    string    `json:"joint,omitempty"`
	Frame    int       `json:"frame,omitempty"`
	HasFrame bool      `json:"-"`
	Op       string    `json:"op,omitempty"`
	Field    string    `json:"field,omitempty"`
	Left     any       `json:"left,omitempty"`
	Right    any       `json:"right,omitempty"`
	Delta    float64   `json:"delta,omitempty"`
	Cause    error     `json:"-"`
}

func (e *CodedError) Error() string {
	msg := string(e.Code)
	if e.Op != "" {
		msg += " " + e.Op
	}
	if e.Joint != "" {
		msg += " joint=" + e.Joint
	}
	if e.HasFrame {
		msg += fmt.Sprintf(" frame=%d", e.Frame)
	}
	if e.Message != "" {
		msg += ": " + e.Message
	}
	return msg
}

func (e *CodedError) Unwrap() error { return e.Cause }

func (e *CodedError) MarshalJSON() ([]byte, error) {
	type wire struct {
		Code    ErrorCode `json:"code"`
		Message string    `json:"message"`
		Joint   string    `json:"joint,omitempty"`
		Frame   *int      `json:"frame,omitempty"`
		Op      string    `json:"op,omitempty"`
		Field   string    `json:"field,omitempty"`
		Left    any       `json:"left,omitempty"`
		Right   any       `json:"right,omitempty"`
		Delta   float64   `json:"delta,omitempty"`
	}
	w := wire{
		Code: e.Code, Message: e.Message, Joint: e.Joint, Op: e.Op,
		Field: e.Field, Left: e.Left, Right: e.Right, Delta: e.Delta,
	}
	if e.HasFrame {
		f := e.Frame
		w.Frame = &f
	}
	return json.Marshal(w)
}

func Err(code ErrorCode, op, message string) *CodedError {
	return &CodedError{Code: code, Op: op, Message: message}
}

func ErrJoint(code ErrorCode, op, joint, message string) *CodedError {
	return &CodedError{Code: code, Op: op, Joint: joint, Message: message}
}

func ErrFrame(code ErrorCode, op string, frame int, joint, message string) *CodedError {
	return &CodedError{
		Code: code, Op: op, Joint: joint, Frame: frame, HasFrame: true, Message: message,
	}
}

func IsCode(err error, code ErrorCode) bool {
	var ce *CodedError
	return errors.As(err, &ce) && ce.Code == code
}
