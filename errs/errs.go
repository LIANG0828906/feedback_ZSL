package errs

import "fmt"

const (
	ESchema   = "E_SCHEMA"
	ELossy    = "E_LOSSY"
	EOvf      = "E_OVF"
	EThr      = "E_THR"
	ERegion   = "E_REGION"
	ETop      = "E_TOP"
	EState    = "E_STATE"
	EDiff     = "E_DIFF"
	EBudget   = "E_BUDGET"
	ECheck    = "E_CHECKPOINT"
	EInternal = "E_INTERNAL"
)

type Error struct {
	Code    string         `json:"code"`
	Field   string         `json:"field,omitempty"`
	Message string         `json:"message"`
	Op1     any            `json:"op1,omitempty"`
	Op2     any            `json:"op2,omitempty"`
	Details map[string]any `json:"details,omitempty"`
	Issues  []*Error       `json:"issues,omitempty"`
}

func (e *Error) Error() string {
	if e == nil {
		return ""
	}
	if e.Field != "" {
		return fmt.Sprintf("%s at %s: %s", e.Code, e.Field, e.Message)
	}
	return fmt.Sprintf("%s: %s", e.Code, e.Message)
}

func (e *Error) Unwrap() []error {
	if e == nil {
		return nil
	}
	out := make([]error, 0, len(e.Issues))
	for _, issue := range e.Issues {
		out = append(out, issue)
	}
	return out
}

func New(code, field, format string, args ...any) *Error {
	return &Error{Code: code, Field: field, Message: fmt.Sprintf(format, args...)}
}

func Schema(field, format string, args ...any) *Error {
	return New(ESchema, field, format, args...)
}

func Ovf(field string, op1, op2 any, format string, args ...any) *Error {
	e := New(EOvf, field, format, args...)
	e.Op1 = op1
	e.Op2 = op2
	return e
}

func State(field, format string, args ...any) *Error {
	return New(EState, field, format, args...)
}

func Diff(field string, format string, args ...any) *Error {
	return New(EDiff, field, format, args...)
}

func AsError(err error) (*Error, bool) {
	if err == nil {
		return nil, false
	}
	if e, ok := err.(*Error); ok {
		return e, true
	}
	return nil, false
}
