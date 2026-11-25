package model

import (
	"errors"
	"fmt"
	"strings"
)

const (
	CodeSchema = "E_SCHEMA"
	CodeDecode = "E_DECODE"
	CodeComp   = "E_COMP"
	CodeOrder  = "E_ORDER"
	CodeNorm   = "E_NORM"
	CodeTopo   = "E_TOPO"
	CodeMap    = "E_MAP"
	CodeBound  = "E_BOUND"
	CodeState  = "E_STATE"
	CodeFrame  = "E_FRAME"
)

// Error carries every location that is useful for deterministic diagnostics.
type Error struct {
	Code    string
	Path    string
	Version string
	Joint   string
	Frame   int
	Message string
	Cause   error
}

func (e *Error) Error() string {
	var b strings.Builder
	b.WriteString(e.Code)
	if e.Path != "" {
		b.WriteString(" path=")
		b.WriteString(e.Path)
	}
	if e.Version != "" {
		b.WriteString(" version=")
		b.WriteString(e.Version)
	}
	if e.Joint != "" {
		b.WriteString(" joint=")
		b.WriteString(e.Joint)
	}
	if e.Frame >= 0 {
		b.WriteString(fmt.Sprintf(" frame=%d", e.Frame))
	}
	if e.Message != "" {
		b.WriteString(": ")
		b.WriteString(e.Message)
	}
	if e.Cause != nil {
		b.WriteString(": ")
		b.WriteString(e.Cause.Error())
	}
	return b.String()
}

func (e *Error) Unwrap() error { return e.Cause }

func NewError(code, format string, args ...any) *Error {
	return &Error{Code: code, Frame: -1, Message: fmt.Sprintf(format, args...)}
}

func IsCode(err error, code string) bool {
	var appErr *Error
	return errors.As(err, &appErr) && appErr.Code == code
}
