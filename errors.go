package quantforensics

import "fmt"

// ErrorCode is the stable forensics error category used by CLI and tests.
type ErrorCode string

const (
	CodeSchema       ErrorCode = "E_SCHEMA"
	CodeLossy        ErrorCode = "E_LOSSY"
	CodeOverflow     ErrorCode = "E_OVF"
	CodeState        ErrorCode = "E_STATE"
	CodeBudget       ErrorCode = "E_BUDGET"
	CodeInconsistent ErrorCode = "E_INCONSISTENT"
	CodeFault        ErrorCode = "E_FAULT"
)

// Error carries a field path and structured evidence. Overflow errors retain
// both operands; state errors retain the attempted transition.
type Error struct {
	Code      ErrorCode `json:"code"`
	Field     string    `json:"field,omitempty"`
	Message   string    `json:"message"`
	Op1       *int64    `json:"op1,omitempty"`
	Op2       *int64    `json:"op2,omitempty"`
	FromState string    `json:"fromState,omitempty"`
	ToState   string    `json:"toState,omitempty"`
}

func (e *Error) Error() string {
	if e == nil {
		return "<nil>"
	}
	switch {
	case e.Field != "" && e.Op1 != nil && e.Op2 != nil:
		return fmt.Sprintf("%s %s: %s (op1=%d op2=%d)", e.Code, e.Field, e.Message, *e.Op1, *e.Op2)
	case e.Field != "":
		return fmt.Sprintf("%s %s: %s", e.Code, e.Field, e.Message)
	default:
		return fmt.Sprintf("%s: %s", e.Code, e.Message)
	}
}

func schemaError(field, format string, args ...any) error {
	return &Error{Code: CodeSchema, Field: field, Message: fmt.Sprintf(format, args...)}
}

func overflowError(field string, a, b int64, format string, args ...any) error {
	aa, bb := a, b
	return &Error{Code: CodeOverflow, Field: field, Message: fmt.Sprintf(format, args...), Op1: &aa, Op2: &bb}
}

func stateError(id string, from, to fmt.Stringer) error {
	return &Error{
		Code:      CodeState,
		Field:     id,
		Message:   "illegal state transition",
		FromState: from.String(),
		ToState:   to.String(),
	}
}
