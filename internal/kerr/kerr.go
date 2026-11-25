package kerr

import (
	"errors"
	"fmt"
	"strings"
)

const (
	CodeVersion  = "E_VER"
	CodeSchema   = "E_SCHEMA"
	CodeUnit     = "E_UNIT"
	CodeOverflow = "E_OVF"
	CodeLossy    = "E_LOSSY"
	CodeDiff     = "E_DIFF"
)

// Error is the stable error envelope used at the public API boundary.
type Error struct {
	Code string
	Path string
	Msg  string
}

func (e *Error) Error() string {
	if e == nil {
		return "<nil>"
	}
	if e.Path == "" {
		return fmt.Sprintf("%s: %s", e.Code, e.Msg)
	}
	return fmt.Sprintf("%s at %s: %s", e.Code, e.Path, e.Msg)
}

func New(code, path, msg string) error {
	return &Error{Code: code, Path: path, Msg: msg}
}

func Overflow(path string) error {
	return New(CodeOverflow, path, "integer conversion overflow")
}

// LossItem describes one value that cannot be represented by a target version.
type LossItem struct {
	Path   string `json:"path"`
	Kind   string `json:"kind"`
	Action string `json:"action"`
	Detail string `json:"detail"`
}

type LossError struct {
	Items []LossItem
}

func (e *LossError) Error() string {
	if e == nil || len(e.Items) == 0 {
		return CodeLossy + ": lossy conversion"
	}
	parts := make([]string, 0, len(e.Items))
	for _, item := range e.Items {
		parts = append(parts, fmt.Sprintf("%s (%s)", item.Path, item.Kind))
	}
	return fmt.Sprintf("%s: %s", CodeLossy, strings.Join(parts, ", "))
}

func AsLossError(err error) (*LossError, bool) {
	var le *LossError
	if errors.As(err, &le) {
		return le, true
	}
	return nil, false
}

func CodeOf(err error) string {
	var e *Error
	if errors.As(err, &e) {
		return e.Code
	}
	var le *LossError
	if errors.As(err, &le) {
		return CodeLossy
	}
	return ""
}
