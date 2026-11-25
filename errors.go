package vectorcore

import (
	"errors"
	"fmt"
)

// Code is the stable error taxonomy exposed by the component.
type Code string

const (
	CodeVersion  Code = "E_VER"
	CodeSchema   Code = "E_SCHEMA"
	CodeOverflow Code = "E_OVF"
	CodeParam    Code = "E_PARA"
	CodeMode     Code = "E_MODE"
	CodeState    Code = "E_STATE"
	CodeDiff     Code = "E_DIFF"
	CodeDecode   Code = "E_DECODE"
	CodeIO       Code = "E_IO"
	CodeLimit    Code = "E_LIMIT"
)

// Error carries enough context for integration code to diagnose malformed
// scenes and invalid pipeline operations without parsing strings.
type Error struct {
	Code          Code
	Op            string
	Field         string
	SourceVersion int
	PathID        string
	Keyframe      int
	TimeIndex     int
	Stage         Stage
	X             int
	Y             int
	Reason        string
	Err           error
}

func (e *Error) Error() string {
	if e == nil {
		return "<nil>"
	}
	msg := string(e.Code)
	if e.Op != "" {
		msg += " " + e.Op
	}
	if e.Field != "" {
		msg += " field=" + e.Field
	}
	if e.SourceVersion != 0 {
		msg += fmt.Sprintf(" source_version=%d", e.SourceVersion)
	}
	if e.PathID != "" {
		msg += " path=" + e.PathID
	}
	if e.Keyframe >= 0 {
		msg += fmt.Sprintf(" keyframe=%d", e.Keyframe)
	}
	if e.TimeIndex >= 0 {
		msg += fmt.Sprintf(" time_index=%d", e.TimeIndex)
	}
	if e.Stage != "" {
		msg += " stage=" + string(e.Stage)
	}
	if e.X != 0 || e.Y != 0 {
		msg += fmt.Sprintf(" pixel=(%d,%d)", e.X, e.Y)
	}
	if e.Reason != "" {
		msg += " reason=" + e.Reason
	}
	if e.Err != nil {
		msg += ": " + e.Err.Error()
	}
	return msg
}

func (e *Error) Unwrap() error { return e.Err }

// CodeOf returns the stable error code when err contains a component error.
func CodeOf(err error) Code {
	var target *Error
	if errors.As(err, &target) {
		return target.Code
	}
	return ""
}

func newError(code Code, op string, err error) *Error {
	return &Error{Code: code, Op: op, Keyframe: -1, TimeIndex: -1, Err: err}
}

func errorf(code Code, op, format string, args ...any) *Error {
	return newError(code, op, fmt.Errorf(format, args...))
}

func withField(err error, field string, version int) error {
	var target *Error
	if errors.As(err, &target) {
		cp := *target
		if cp.Field == "" {
			cp.Field = field
		}
		if cp.SourceVersion == 0 {
			cp.SourceVersion = version
		}
		return &cp
	}
	return &Error{Code: CodeDecode, Op: "decode", Field: field, SourceVersion: version, Keyframe: -1, TimeIndex: -1, Err: err}
}
