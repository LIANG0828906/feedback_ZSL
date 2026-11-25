package channelpipe

import (
	"errors"
	"fmt"
)

const (
	CodeVersion    = "E_VERSION"
	CodeSchema     = "E_SCHEMA"
	CodeLossy      = "E_LOSSY"
	CodeState      = "E_STATE"
	CodeDiff       = "E_DIFF"
	CodeCheckpoint = "E_CHECKPOINT"
	CodeInvalid    = "E_INVALID"
	CodeIO         = "E_IO"
)

type Error struct {
	Code    string         `json:"code"`
	Message string         `json:"message"`
	Field   string         `json:"field,omitempty"`
	Details map[string]any `json:"details,omitempty"`
	Err     error          `json:"-"`
}

func (e *Error) Error() string {
	if e.Field != "" {
		return fmt.Sprintf("%s: %s (field=%s)", e.Code, e.Message, e.Field)
	}
	return fmt.Sprintf("%s: %s", e.Code, e.Message)
}

func (e *Error) Unwrap() error { return e.Err }

func NewError(code, format string, args ...any) *Error {
	return &Error{Code: code, Message: fmt.Sprintf(format, args...)}
}

func FieldError(code, field, format string, args ...any) *Error {
	return &Error{Code: code, Field: field, Message: fmt.Sprintf(format, args...)}
}

func WrapError(code string, err error, field, format string, args ...any) *Error {
	return &Error{
		Code:    code,
		Field:   field,
		Message: fmt.Sprintf(format, args...),
		Err:     err,
	}
}

func AsError(err error) (*Error, bool) {
	var target *Error
	if errors.As(err, &target) {
		return target, true
	}
	return nil, false
}

func IsCode(err error, code string) bool {
	domainErr, ok := AsError(err)
	return ok && domainErr.Code == code
}
