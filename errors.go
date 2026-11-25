package audiofp

import (
	"errors"
	"fmt"
	"strings"
)

const (
	CodeSchema = "E_SCHEMA"
	CodeRange  = "E_RANGE"
	CodePara   = "E_PARA"
	CodeQual   = "E_QUAL"
	CodeState  = "E_STATE"
	CodeDiff   = "E_DIFF"
)

// Error is the structured domain error used by all components.
type Error struct {
	Code       string `json:"code"`
	Message    string `json:"message"`
	Field      string `json:"field,omitempty"`
	EventIndex int    `json:"event_index,omitempty"`
	EventNo    int    `json:"event_no,omitempty"`
	FrameIndex int    `json:"frame_index,omitempty"`
	Expected   any    `json:"expected,omitempty"`
	Actual     any    `json:"actual,omitempty"`
	Details    any    `json:"details,omitempty"`
}

func (e *Error) Error() string {
	var b strings.Builder
	b.WriteString(e.Code)
	if e.Field != "" {
		fmt.Fprintf(&b, " field=%s", e.Field)
	}
	if e.EventNo > 0 {
		fmt.Fprintf(&b, " event=#%d", e.EventNo)
	}
	if e.FrameIndex > 0 || (e.FrameIndex == 0 && strings.Contains(e.Message, "frame")) {
		fmt.Fprintf(&b, " frame=#%d", e.FrameIndex)
	}
	if e.Message != "" {
		b.WriteString(": ")
		b.WriteString(e.Message)
	}
	return b.String()
}

func (e *Error) Is(target error) bool {
	var t *Error
	if !errors.As(target, &t) {
		return false
	}
	return t.Code == "" || t.Code == e.Code
}

func newError(code, field, format string, args ...any) *Error {
	return &Error{Code: code, Field: field, Message: fmt.Sprintf(format, args...)}
}

// NewError creates a structured domain error for adapters.
func NewError(code, field, format string, args ...any) *Error {
	return newError(code, field, format, args...)
}

// NewHTTPError is an adapter-friendly alias for NewError.
func NewHTTPError(code, field, format string, args ...any) *Error {
	return NewError(code, field, format, args...)
}

func schemaError(field, format string, args ...any) *Error {
	return newError(CodeSchema, field, format, args...)
}

func paraError(field, format string, args ...any) *Error {
	return newError(CodePara, field, format, args...)
}

func rangeError(field string, eventNo int, format string, args ...any) *Error {
	e := newError(CodeRange, field, format, args...)
	e.EventNo = eventNo
	if eventNo > 0 {
		e.EventIndex = eventNo - 1
	}
	return e
}

func stateError(index int, from, to WindowState) *Error {
	e := newError(CodeState, fmt.Sprintf("windows[%d].state", index),
		"illegal transition %s -> %s", from, to)
	e.FrameIndex = index
	e.Actual = from
	e.Expected = allowedTransitions[from]
	return e
}

// AsError returns the first *Error in err's tree, if present.
func AsError(err error) (*Error, bool) {
	var target *Error
	if errors.As(err, &target) {
		return target, true
	}
	return nil, false
}

// HasCode reports whether err contains a domain error with code.
func HasCode(err error, code string) bool {
	var target *Error
	if errors.As(err, &target) {
		return target.Code == code
	}
	return false
}
