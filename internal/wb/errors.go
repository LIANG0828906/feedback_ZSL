package wb

import (
	"errors"
	"fmt"
)

const (
	CodeSchema   = "E_SCHEMA"
	CodeGeom     = "E_GEOM"
	CodeConn     = "E_CONN"
	CodePara     = "E_PARA"
	CodeState    = "E_STATE"
	CodeDiff     = "E_DIFF"
	CodeInternal = "E_INTERNAL"
)

// APIError is the single error envelope used by HTTP JSON and stderr logs.
type APIError struct {
	Code    string `json:"code"`
	Message string `json:"message"`
	Object  string `json:"object,omitempty"`
	Field   string `json:"field,omitempty"`
	Details any    `json:"details,omitempty"`
}

func (e *APIError) Error() string {
	if e.Object != "" {
		return fmt.Sprintf("%s object=%s: %s", e.Code, e.Object, e.Message)
	}
	return fmt.Sprintf("%s: %s", e.Code, e.Message)
}

func NewAPIError(code, object, field, format string, args ...any) *APIError {
	return &APIError{
		Code:    code,
		Object:  object,
		Field:   field,
		Message: fmt.Sprintf(format, args...),
	}
}

func AsAPIError(err error) (*APIError, bool) {
	var apiErr *APIError
	if errors.As(err, &apiErr) {
		return apiErr, true
	}
	return nil, false
}
