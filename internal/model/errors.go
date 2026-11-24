package model

import (
	"errors"
	"fmt"
)

const (
	EVER     = "E_VER"
	EDECODE  = "E_DECODE"
	EPARA    = "E_PARA"
	ESTATE   = "E_STATE"
	EDIVERGE = "E_DIVERGE"
	EDIFF    = "E_DIFF"
	ELOSSY   = "E_LOSSY"
)

type AppError struct {
	Code    string         `json:"code"`
	Message string         `json:"message"`
	Path    string         `json:"path,omitempty"`
	Offset  int64          `json:"offset,omitempty"`
	Details map[string]any `json:"details,omitempty"`
	Err     error          `json:"-"`
}

func (e *AppError) Error() string {
	if e.Path != "" {
		return fmt.Sprintf("%s: %s (path=%s offset=%d)", e.Code, e.Message, e.Path, e.Offset)
	}
	return fmt.Sprintf("%s: %s", e.Code, e.Message)
}

func (e *AppError) Unwrap() error {
	return e.Err
}

func NewError(code, message, path string, offset int64, details map[string]any) *AppError {
	return &AppError{Code: code, Message: message, Path: path, Offset: offset, Details: details}
}

func AsAppError(err error) *AppError {
	if err == nil {
		return nil
	}
	var appErr *AppError
	if errors.As(err, &appErr) {
		return appErr
	}
	return &AppError{Code: EDECODE, Message: err.Error(), Err: err}
}

type Warning struct {
	Code      string `json:"code"`
	Severity  string `json:"severity"`
	Message   string `json:"message"`
	Path      string `json:"path,omitempty"`
	Transform string `json:"transform,omitempty"`
}
