package core

import (
	"errors"
	"fmt"
	"sort"
	"strings"
)

const (
	CodeDecode   = "E_DECODE"
	CodeMeta     = "E_META"
	CodeParam    = "E_PARA"
	CodeRegion   = "E_REGION"
	CodeVersion  = "E_VER"
	CodeDiverge  = "E_DIVERGE"
	CodeState    = "E_STATE"
	CodeResume   = "E_RESUME"
	CodeResource = "E_RESOURCE"
)

// Error is the structured error returned by all public subsystems. Keeping the
// machine-readable code separate from the message makes CLI and test handling
// deterministic while still exposing the two values involved in a mismatch.
type Error struct {
	Code    string         `json:"code"`
	Op      string         `json:"op,omitempty"`
	Field   string         `json:"field,omitempty"`
	Got     any            `json:"got,omitempty"`
	Want    any            `json:"want,omitempty"`
	Left    string         `json:"left,omitempty"`
	Right   string         `json:"right,omitempty"`
	Message string         `json:"message,omitempty"`
	Data    map[string]any `json:"data,omitempty"`
}

func (e *Error) Error() string {
	var b strings.Builder
	b.WriteString(e.Code)
	if e.Op != "" {
		b.WriteString(" [")
		b.WriteString(e.Op)
		b.WriteString("]")
	}
	if e.Field != "" {
		b.WriteString(" field=")
		b.WriteString(e.Field)
	}
	if e.Got != nil || e.Want != nil {
		b.WriteString(" got=")
		b.WriteString(fmt.Sprint(e.Got))
		b.WriteString(" want=")
		b.WriteString(fmt.Sprint(e.Want))
	}
	if e.Left != "" || e.Right != "" {
		b.WriteString(" left=")
		b.WriteString(e.Left)
		b.WriteString(" right=")
		b.WriteString(e.Right)
	}
	if len(e.Data) > 0 {
		keys := make([]string, 0, len(e.Data))
		for k := range e.Data {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		for _, k := range keys {
			b.WriteString(" ")
			b.WriteString(k)
			b.WriteString("=")
			b.WriteString(fmt.Sprint(e.Data[k]))
		}
	}
	if e.Message != "" {
		b.WriteString(": ")
		b.WriteString(e.Message)
	}
	return b.String()
}

func NewError(code, op, message string) *Error {
	return &Error{Code: code, Op: op, Message: message}
}

func MetaError(op, field string, got, want any) *Error {
	return &Error{Code: CodeMeta, Op: op, Field: field, Got: got, Want: want, Message: "declaration mismatch"}
}

func ParamError(op, field string, got, min, max any) *Error {
	return &Error{
		Code:    CodeParam,
		Op:      op,
		Field:   field,
		Got:     got,
		Want:    fmt.Sprintf("[%v,%v]", min, max),
		Message: "parameter outside legal interval",
		Data:    map[string]any{"min": min, "max": max},
	}
}

func VersionError(op string, left, right string) *Error {
	return &Error{
		Code:    CodeVersion,
		Op:      op,
		Left:    left,
		Right:   right,
		Message: "parameter snapshot version mismatch",
		Data:    map[string]any{"preview_version": left, "exact_version": right},
	}
}

func CodeOf(err error) string {
	var e *Error
	if errors.As(err, &e) {
		return e.Code
	}
	return ""
}
