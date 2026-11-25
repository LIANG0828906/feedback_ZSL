package iconcut

import "fmt"

type ErrorCode string

const (
	EDecode   ErrorCode = "E_DECODE"
	EMeta     ErrorCode = "E_META"
	EGeom     ErrorCode = "E_GEOM"
	EBand     ErrorCode = "E_BAND"
	EPara     ErrorCode = "E_PARA"
	ERegion   ErrorCode = "E_REGION"
	EDiverge  ErrorCode = "E_DIVERGE"
	EState    ErrorCode = "E_STATE"
	ESeed     ErrorCode = "E_SEED"
	EGrow     ErrorCode = "E_GROW"
	EEmpty    ErrorCode = "E_EMPTY"
	EInternal ErrorCode = "E_INTERNAL"
)

type Error struct {
	Code    ErrorCode      `json:"code"`
	Field   string         `json:"field,omitempty"`
	Message string         `json:"message"`
	Details map[string]any `json:"details,omitempty"`
}

func (e *Error) Error() string {
	if e.Field != "" {
		return fmt.Sprintf("%s at %s: %s", e.Code, e.Field, e.Message)
	}
	return fmt.Sprintf("%s: %s", e.Code, e.Message)
}

func NewError(code ErrorCode, field, format string, args ...any) *Error {
	return &Error{Code: code, Field: field, Message: fmt.Sprintf(format, args...)}
}

func WithDetails(err *Error, details map[string]any) *Error {
	if err == nil {
		return nil
	}
	if err.Details == nil {
		err.Details = map[string]any{}
	}
	for k, v := range details {
		err.Details[k] = v
	}
	return err
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
