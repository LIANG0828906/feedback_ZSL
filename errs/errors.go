// Package errs defines the stable business errors emitted by the workbench.
package errs

import (
	"encoding/json"
	"errors"
	"fmt"
)

const (
	CodePlane     = "E_PLANE"
	CodeParameter = "E_PARA"
	CodeDiff      = "E_DIFF"
	CodePath      = "E_PATH"
	CodeSnapshot  = "E_SNAPSHOT"
	CodeStage     = "E_STAGE"
	CodeCancel    = "E_CANCEL"
	CodeResource  = "E_RESOURCE"
	CodeVerify    = "E_VERIFY"
	CodeIO        = "E_IO"
)

// Error is the JSON-serializable error shape used by the CLI and public API.
type Error struct {
	Code         string  `json:"code"`
	Message      string  `json:"message"`
	Field        string  `json:"field,omitempty"`
	Plane        string  `json:"plane,omitempty"`
	Stage        string  `json:"stage,omitempty"`
	Node         string  `json:"node,omitempty"`
	Expected     any     `json:"expected,omitempty"`
	Actual       any     `json:"actual,omitempty"`
	Left         any     `json:"left,omitempty"`
	Right        any     `json:"right,omitempty"`
	X            *int    `json:"x,omitempty"`
	Y            *int    `json:"y,omitempty"`
	LeftValue    *uint16 `json:"leftValue,omitempty"`
	RightValue   *uint16 `json:"rightValue,omitempty"`
	LeftSummary  string  `json:"leftSummary,omitempty"`
	RightSummary string  `json:"rightSummary,omitempty"`
	Cause        string  `json:"cause,omitempty"`
}

func (e *Error) Error() string {
	if e == nil {
		return ""
	}
	b, err := json.Marshal(e)
	if err != nil {
		return fmt.Sprintf("%s: %s", e.Code, e.Message)
	}
	return string(b)
}

func New(code, message string) *Error {
	return &Error{Code: code, Message: message}
}

func WithField(code, field, message string) *Error {
	return &Error{Code: code, Field: field, Message: message}
}

func As(err error) (*Error, bool) {
	var target *Error
	if errors.As(err, &target) {
		return target, true
	}
	return nil, false
}

func CodeOf(err error) string {
	if e, ok := As(err); ok {
		return e.Code
	}
	return ""
}
