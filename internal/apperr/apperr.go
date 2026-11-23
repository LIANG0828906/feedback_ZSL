package apperr

import (
	"errors"
	"fmt"
	"sort"
	"strings"
)

const (
	CodeDecode   = "E_DECODE"
	CodeMeta     = "E_META"
	CodePara     = "E_PARA"
	CodeDiverge  = "E_DIVERGE"
	CodeVersion  = "E_VER"
	CodeState    = "E_STATE"
	CodeSeed     = "E_SEED"
	CodeSpill    = "E_SPILL"
	CodeBlock    = "E_BLOCK"
	CodeLatency  = "E_LATENCY"
	CodeIO       = "E_IO"
	CodeInternal = "E_INTERNAL"
)

// Error is the stable machine-readable error contract used by the CLI and tests.
type Error struct {
	Code    string         `json:"code"`
	Op      string         `json:"op,omitempty"`
	Field   string         `json:"field,omitempty"`
	Message string         `json:"message"`
	Details map[string]any `json:"details,omitempty"`
	Err     error          `json:"-"`
}

func (e *Error) Error() string {
	if e == nil {
		return "<nil>"
	}
	var b strings.Builder
	b.WriteString(e.Code)
	if e.Field != "" {
		b.WriteString(" field=")
		b.WriteString(e.Field)
	}
	if e.Message != "" {
		b.WriteString(": ")
		b.WriteString(e.Message)
	}
	if len(e.Details) > 0 {
		keys := make([]string, 0, len(e.Details))
		for k := range e.Details {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		b.WriteString(" [")
		for i, k := range keys {
			if i > 0 {
				b.WriteString(", ")
			}
			fmt.Fprintf(&b, "%s=%v", k, e.Details[k])
		}
		b.WriteString("]")
	}
	if e.Err != nil {
		b.WriteString(": ")
		b.WriteString(e.Err.Error())
	}
	return b.String()
}

func (e *Error) Unwrap() error { return e.Err }

func New(code, op, format string, args ...any) *Error {
	return &Error{
		Code:    code,
		Op:      op,
		Message: fmt.Sprintf(format, args...),
	}
}

func Wrap(code, op, format string, err error, args ...any) *Error {
	e := New(code, op, format, args...)
	e.Err = err
	return e
}

func (e *Error) WithField(field string) *Error {
	e.Field = field
	return e
}

func (e *Error) WithDetail(key string, value any) *Error {
	if e.Details == nil {
		e.Details = make(map[string]any)
	}
	e.Details[key] = value
	return e
}

func (e *Error) WithDetails(details map[string]any) *Error {
	if len(details) == 0 {
		return e
	}
	if e.Details == nil {
		e.Details = make(map[string]any, len(details))
	}
	for k, v := range details {
		e.Details[k] = v
	}
	return e
}

func As(err error) (*Error, bool) {
	var target *Error
	if errors.As(err, &target) {
		return target, true
	}
	return nil, false
}

func Code(err error) string {
	if err == nil {
		return ""
	}
	if e, ok := As(err); ok {
		return e.Code
	}
	return CodeInternal
}

func Field(err error) string {
	if e, ok := As(err); ok {
		return e.Field
	}
	return ""
}
