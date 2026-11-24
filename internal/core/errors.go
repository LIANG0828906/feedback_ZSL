package core

import (
	"errors"
	"fmt"
)

const (
	CodeDecode     = "E_DECODE"
	CodeMeta       = "E_META"
	CodeParam      = "E_PARA"
	CodeDiverge    = "E_DIVERGE"
	CodeState      = "E_STATE"
	CodeCheckpoint = "E_CHECKPOINT"
	CodeResource   = "E_RESOURCE"
	CodeCanceled   = "E_CANCELED"
)

// AppError is the stable error envelope consumed by both tests and the UI.
type AppError struct {
	Code    string         `json:"code"`
	Field   string         `json:"field,omitempty"`
	Message string         `json:"message"`
	Details map[string]any `json:"details,omitempty"`
	Err     error          `json:"-"`
}

func (e *AppError) Error() string {
	if e.Field != "" {
		return fmt.Sprintf("%s (%s): %s", e.Code, e.Field, e.Message)
	}
	return fmt.Sprintf("%s: %s", e.Code, e.Message)
}

func (e *AppError) Unwrap() error { return e.Err }

func NewError(code, field, message string) *AppError {
	return &AppError{Code: code, Field: field, Message: message}
}

func WrapError(code, field, message string, err error) *AppError {
	return &AppError{Code: code, Field: field, Message: message, Err: err}
}

func CodeOf(err error) string {
	var appErr *AppError
	if errors.As(err, &appErr) {
		return appErr.Code
	}
	return ""
}

func FieldOf(err error) string {
	var appErr *AppError
	if errors.As(err, &appErr) {
		return appErr.Field
	}
	return ""
}
