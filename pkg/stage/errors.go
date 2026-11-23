package stage

import (
	"errors"
	"fmt"
)

type ErrorCode string

const (
	CodeSchema    ErrorCode = "E_SCHEMA"
	CodeReference ErrorCode = "E_REF"
	CodeCycle     ErrorCode = "E_CYCLE"
	CodeParameter ErrorCode = "E_PARA"
	CodeScale     ErrorCode = "E_SCALE"
	CodeConflict  ErrorCode = "E_CONFLICT"
	CodeState     ErrorCode = "E_STATE"
	CodeDiverge   ErrorCode = "E_DIVERGE"
	CodeCommitted ErrorCode = "E_COMMITTED"
	CodeStorage   ErrorCode = "E_STORAGE"
)

type ErrorDetail struct {
	Object   string `json:"object,omitempty"`
	Field    string `json:"field,omitempty"`
	Expected string `json:"expected,omitempty"`
	Actual   string `json:"actual,omitempty"`
	Frame    int    `json:"frame,omitempty"`
	TimeMS   int64  `json:"time_ms,omitempty"`
	LightID  string `json:"light_id,omitempty"`
	Window   int    `json:"window,omitempty"`
	Left     string `json:"left,omitempty"`
	Right    string `json:"right,omitempty"`
	Reason   string `json:"reason,omitempty"`
}

type DomainError struct {
	Code    ErrorCode     `json:"code"`
	Message string        `json:"message"`
	Object  string        `json:"object,omitempty"`
	Field   string        `json:"field,omitempty"`
	Cause   string        `json:"cause,omitempty"`
	Details []ErrorDetail `json:"details,omitempty"`
}

func (e *DomainError) Error() string {
	if e == nil {
		return ""
	}
	if e.Object != "" || e.Field != "" {
		return fmt.Sprintf("%s: %s (object=%s field=%s)", e.Code, e.Message, e.Object, e.Field)
	}
	return fmt.Sprintf("%s: %s", e.Code, e.Message)
}

func newError(code ErrorCode, message, object, field string) *DomainError {
	return &DomainError{Code: code, Message: message, Object: object, Field: field}
}

func withDetail(err *DomainError, detail ErrorDetail) *DomainError {
	err.Details = append(err.Details, detail)
	return err
}

func AsDomainError(err error) (*DomainError, bool) {
	var target *DomainError
	if errors.As(err, &target) {
		return target, true
	}
	return nil, false
}
