package engine

import (
	"errors"
	"fmt"
)

// Code is the stable machine-readable error family used by the prototype.
type Code string

const (
	CodeSelf   Code = "E_SELF"
	CodeVer    Code = "E_VER"
	CodeSchema Code = "E_SCHEMA"
	CodeRT     Code = "E_RT"
	CodePara   Code = "E_PARA"
	CodeComp   Code = "E_COMP"
	CodeScale  Code = "E_SCALE"
	CodeLossy  Code = "E_LOSSY"
	CodeEq     Code = "E_EQ"
	CodeState  Code = "E_STATE"
	CodeDiff   Code = "E_DIFF"
)

// Pixel identifies a single bitmap coordinate in diagnostics.
type Pixel struct {
	X int `json:"x"`
	Y int `json:"y"`
}

// Error carries the field path and the context needed by the diagnostic UI.
type Error struct {
	Code    Code    `json:"code"`
	Path    string  `json:"path,omitempty"`
	Field   string  `json:"field,omitempty"`
	Message string  `json:"message,omitempty"`
	Left    string  `json:"left,omitempty"`
	Right   string  `json:"right,omitempty"`
	Frame   int     `json:"frame,omitempty"`
	Pixel   *Pixel  `json:"pixel,omitempty"`
	Metric  string  `json:"metric,omitempty"`
	Actual  float64 `json:"actual,omitempty"`
	Limit   float64 `json:"limit,omitempty"`
}

func (e *Error) Error() string {
	msg := string(e.Code)
	if e.Path != "" {
		msg += " at " + e.Path
	}
	if e.Field != "" && e.Field != e.Path {
		msg += " field=" + e.Field
	}
	if e.Message != "" {
		msg += ": " + e.Message
	}
	if e.Left != "" || e.Right != "" {
		msg += fmt.Sprintf(" (left=%s right=%s)", e.Left, e.Right)
	}
	if e.Frame > 0 {
		msg += fmt.Sprintf(" frame=%d", e.Frame)
	}
	if e.Pixel != nil {
		msg += fmt.Sprintf(" pixel=(%d,%d)", e.Pixel.X, e.Pixel.Y)
	}
	if e.Metric != "" {
		msg += fmt.Sprintf(" %s=%.8f limit=%.8f", e.Metric, e.Actual, e.Limit)
	}
	return msg
}

func errAt(code Code, path, message string) *Error {
	return &Error{Code: code, Path: path, Message: message}
}

// ErrorCode returns the stable code of an engine error, or an empty code.
func ErrorCode(err error) Code {
	var e *Error
	if errors.As(err, &e) {
		return e.Code
	}
	return ""
}

// Warning describes an intentional lossy downgrade during export.
type Warning struct {
	Code        Code   `json:"code"`
	Path        string `json:"path"`
	Original    string `json:"original"`
	Disposition string `json:"disposition"`
}

func (w Warning) Error() string {
	return fmt.Sprintf("%s at %s: %s -> %s", w.Code, w.Path, w.Original, w.Disposition)
}
