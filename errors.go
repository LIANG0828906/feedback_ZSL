package dialoguecal

import (
	"errors"
	"fmt"
)

// Code is the stable machine-readable error code used by the kernel.
type Code string

const (
	CodeSchema   Code = "E_SCHEMA"
	CodeLossy    Code = "E_LOSSY"
	CodeOverflow Code = "E_OVF"
	CodeLoud     Code = "E_LOUD"
	CodeState    Code = "E_STATE"
	CodeDiff     Code = "E_DIFF"
	CodeDecode   Code = "E_DECODE"
	CodeOrder    Code = "E_ORDER"
	CodeCal      Code = "E_CAL"
	CodeStage    Code = "E_STAGE"
	CodeCancel   Code = "E_CANCEL"
	CodeBudget   Code = "E_BUDGET"
	CodeIO       Code = "E_IO"
)

// Error is deliberately a value type: callers can use errors.As and still
// obtain every locator needed for failure diagnostics.
type Error struct {
	Code    Code
	Path    string
	Field   string
	Message string

	SegmentID string
	Frame     int
	HasFrame  bool

	Target         int64
	ReachableUpper int64
	OperandA       int64
	OperandB       int64

	Entity string
	From   string
	To     string

	FirstSegmentID string
	FirstFrame     int
	HasFirstFrame  bool
}

func (e *Error) Error() string {
	if e == nil {
		return ""
	}
	s := string(e.Code)
	if e.Path != "" {
		s += " path=" + e.Path
	}
	if e.Field != "" {
		s += " field=" + e.Field
	}
	if e.SegmentID != "" {
		s += " segment=" + e.SegmentID
	}
	if e.HasFrame {
		s += fmt.Sprintf(" frame=%d", e.Frame)
	}
	if e.Message != "" {
		s += ": " + e.Message
	}
	return s
}

func errf(code Code, path, format string, args ...any) *Error {
	return &Error{Code: code, Path: path, Message: fmt.Sprintf(format, args...)}
}

// AsError extracts a *Error from err, returning nil when err is not a kernel
// error.
func AsError(err error) *Error {
	if err == nil {
		return nil
	}
	var e *Error
	if errors.As(err, &e) {
		return e
	}
	return nil
}
