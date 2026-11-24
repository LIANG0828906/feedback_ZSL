// Package apperr defines the stable error codes and machine-readable context
// shared by the CLI, desktop application, tests and audit tooling.
package apperr

import (
	"errors"
	"fmt"
	"strings"
)

// Code is a stable, externally visible error category.
type Code string

const (
	CodeDecode Code = "E_DECODE"
	CodeMeta   Code = "E_META"
	CodePara   Code = "E_PARA"
	CodeState  Code = "E_STATE"
	CodeFrame  Code = "E_FRAME"
	CodeSeg    Code = "E_SEG"
	CodeDiff   Code = "E_DIFF"
	CodeAudit  Code = "E_AUDIT"
)

// Error carries enough context for an operator or a third party to locate the
// exact failing route, segment, frame and field. Segment and Frame use -1 when
// the value is not applicable.
type Error struct {
	Code     Code
	RouteID  string
	Segment  int
	Frame    int
	Field    string
	Expected string
	Message  string
	Err      error
}

func (e *Error) Error() string {
	if e == nil {
		return "<nil>"
	}
	var b strings.Builder
	b.WriteString(string(e.Code))
	if e.RouteID != "" {
		fmt.Fprintf(&b, " route=%s", e.RouteID)
	}
	if e.Segment >= 0 {
		fmt.Fprintf(&b, " segment=%d", e.Segment)
	}
	if e.Frame >= 0 {
		fmt.Fprintf(&b, " frame=%d", e.Frame)
	}
	if e.Field != "" {
		fmt.Fprintf(&b, " field=%s", e.Field)
	}
	if e.Expected != "" {
		fmt.Fprintf(&b, " expected=%s", e.Expected)
	}
	if e.Message != "" {
		fmt.Fprintf(&b, ": %s", e.Message)
	}
	return b.String()
}

func (e *Error) Unwrap() error { return e.Err }

// New builds a contextual error.
func New(code Code, route string, segment, frame int, field, expected, message string, cause error) *Error {
	return &Error{
		Code: code, RouteID: route, Segment: segment, Frame: frame,
		Field: field, Expected: expected, Message: message, Err: cause,
	}
}

// CodeOf returns the stable category of err, or an empty code for other errors.
func CodeOf(err error) Code {
	var target *Error
	if errors.As(err, &target) {
		return target.Code
	}
	return ""
}

// As returns the first contextual error in err's chain.
func As(err error) (*Error, bool) {
	var target *Error
	ok := errors.As(err, &target)
	return target, ok
}

type Info struct {
	Code     string `json:"code"`
	RouteID  string `json:"route_id,omitempty"`
	Segment  int    `json:"segment,omitempty"`
	Frame    int    `json:"frame,omitempty"`
	Field    string `json:"field,omitempty"`
	Expected string `json:"expected,omitempty"`
	Message  string `json:"message,omitempty"`
}

// Describe flattens an error for JSON reports and the UI.
func Describe(err error) Info {
	if target, ok := As(err); ok {
		return Info{
			Code: string(target.Code), RouteID: target.RouteID,
			Segment: target.Segment, Frame: target.Frame, Field: target.Field,
			Expected: target.Expected, Message: target.Message,
		}
	}
	if err == nil {
		return Info{}
	}
	return Info{Message: err.Error()}
}
