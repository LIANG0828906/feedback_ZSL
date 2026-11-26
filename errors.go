package maskqa

import (
	"errors"
	"fmt"
)

// Business error codes used by the workbench.
const (
	CodeDecode = "E_DECODE"
	CodeLossy  = "E_LOSSY"
	CodeOvf    = "E_OVF"
	CodePara   = "E_PARA"
	CodeTop    = "E_TOP"
	CodeDiff   = "E_DIFF"
)

// Error is the structured business error returned by the domain package.
type Error struct {
	Code     string        `json:"code"`
	Field    string        `json:"field,omitempty"`
	Message  string        `json:"message"`
	OpA      int64         `json:"op_a,omitempty"`
	OpB      int64         `json:"op_b,omitempty"`
	Min      int64         `json:"min,omitempty"`
	Max      int64         `json:"max,omitempty"`
	DomainNo int           `json:"domain_no,omitempty"`
	Expected any           `json:"expected,omitempty"`
	Actual   any           `json:"actual,omitempty"`
	Location *DiffLocation `json:"location,omitempty"`
}

func (e *Error) Error() string {
	if e == nil {
		return ""
	}
	if e.Field != "" {
		return fmt.Sprintf("%s %s: %s", e.Code, e.Field, e.Message)
	}
	return fmt.Sprintf("%s: %s", e.Code, e.Message)
}

// AsError extracts *Error from err.
func AsError(err error) (*Error, bool) {
	var target *Error
	if errors.As(err, &target) {
		if target == nil {
			return nil, false
		}
		return target, true
	}
	return nil, false
}

func decodeError(field, format string, args ...any) error {
	return &Error{Code: CodeDecode, Field: field, Message: fmt.Sprintf(format, args...)}
}

func paraError(field string, min, max int64, format string, args ...any) error {
	return &Error{
		Code:    CodePara,
		Field:   field,
		Message: fmt.Sprintf(format, args...),
		Min:     min,
		Max:     max,
	}
}

func ovfError(field string, a, b int64) error {
	return &Error{
		Code:    CodeOvf,
		Field:   field,
		Message: "integer conversion overflow",
		OpA:     a,
		OpB:     b,
	}
}
