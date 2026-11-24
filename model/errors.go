package model

import (
	"errors"
	"fmt"
	"strings"
)

// Code is a stable machine-readable error code.
type Code string

const (
	EVer    Code = "E_VER"
	ESchema Code = "E_SCHEMA"
	ELossy  Code = "E_LOSSY"
	EOvf    Code = "E_OVF"
	EDiff   Code = "E_DIFF"
	EUnit   Code = "E_UNIT"
	ERes    Code = "E_RES"
	EWindow Code = "E_WINDOW"
	EStore  Code = "E_STORE"
	ECancel Code = "E_CANCELED"
	EIO     Code = "E_IO"
	EFailed Code = "E_WINDOW_FAILED"
)

// Error is the typed error returned across the loader, converter, settlement
// and checkpoint boundaries.
type Error struct {
	Code      Code   `json:"code"`
	Path      string `json:"path,omitempty"`
	NodeID    string `json:"nodeId,omitempty"`
	Field     string `json:"field,omitempty"`
	Window    int    `json:"window"`
	HasWindow bool   `json:"hasWindow,omitempty"`
	Resource  string `json:"resource,omitempty"`
	Message   string `json:"message,omitempty"`
	Losses    []Loss `json:"losses,omitempty"`
	Diff      *Diff  `json:"diff,omitempty"`
	Cause     error  `json:"-"`
}

func (e *Error) Error() string {
	if e == nil {
		return "<nil>"
	}
	var b strings.Builder
	b.WriteString(string(e.Code))
	if e.Path != "" {
		b.WriteString(" path=")
		b.WriteString(e.Path)
	}
	if e.HasWindow {
		fmt.Fprintf(&b, " window=%d", e.Window)
	}
	if e.NodeID != "" {
		b.WriteString(" node=")
		b.WriteString(e.NodeID)
	}
	if e.Field != "" {
		b.WriteString(" field=")
		b.WriteString(e.Field)
	}
	if e.Resource != "" {
		b.WriteString(" resource=")
		b.WriteString(e.Resource)
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

// NewError constructs a typed error.
func NewError(code Code, path, message string) *Error {
	return &Error{Code: code, Path: path, Message: message}
}

// WithNode adds node/field context.
func (e *Error) WithNode(nodeID, field string) *Error {
	e.NodeID = nodeID
	e.Field = field
	return e
}

// WithWindow adds window context.
func (e *Error) WithWindow(window int) *Error {
	e.Window = window
	e.HasWindow = true
	return e
}

// WithCause attaches an underlying error.
func (e *Error) WithCause(err error) *Error {
	e.Cause = err
	return e
}

// AsError extracts a typed model error.
func AsError(err error) (*Error, bool) {
	var target *Error
	if errors.As(err, &target) {
		return target, true
	}
	return nil, false
}

// IsCode reports whether err carries code.
func IsCode(err error, code Code) bool {
	if typed, ok := AsError(err); ok {
		return typed.Code == code
	}
	return false
}

// JSONPointer joins and escapes JSON pointer segments.
func JSONPointer(parts ...string) string {
	if len(parts) == 0 {
		return ""
	}
	escaped := make([]string, 0, len(parts))
	for _, part := range parts {
		if part == "" {
			continue
		}
		part = strings.ReplaceAll(part, "~", "~0")
		part = strings.ReplaceAll(part, "/", "~1")
		escaped = append(escaped, part)
	}
	if len(escaped) == 0 {
		return ""
	}
	return "/" + strings.Join(escaped, "/")
}
