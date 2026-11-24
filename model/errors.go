package model

import (
	"errors"
	"fmt"
	"strings"
)

type ErrorCode string

const (
	CodeVersion ErrorCode = "E_VER"
	CodeCycle   ErrorCode = "E_CYCLE"
	CodeParam   ErrorCode = "E_PARA"
	CodeState   ErrorCode = "E_STATE"
	CodeLossy   ErrorCode = "E_LOSSY"
	CodeDecode  ErrorCode = "E_DECODE"
	CodeIO      ErrorCode = "E_IO"
	CodeCompare ErrorCode = "E_COMPARE"
	CodeCancel  ErrorCode = "E_CANCEL"
)

// Error is the stable, machine-readable failure type used across the pipeline.
// Path points at the source field or the frozen model field that caused it.
type Error struct {
	Code    ErrorCode
	Path    string
	LayerID string
	Detail  string
	Cause   error
}

func (e *Error) Error() string {
	parts := []string{string(e.Code)}
	if e.Path != "" {
		parts = append(parts, "path="+e.Path)
	}
	if e.LayerID != "" {
		parts = append(parts, "layer="+e.LayerID)
	}
	if e.Detail != "" {
		parts = append(parts, e.Detail)
	}
	if e.Cause != nil {
		parts = append(parts, "cause="+e.Cause.Error())
	}
	return strings.Join(parts, ": ")
}

func (e *Error) Unwrap() error { return e.Cause }

func NewError(code ErrorCode, path, detail string) *Error {
	return &Error{Code: code, Path: path, Detail: detail}
}

func WrapError(code ErrorCode, path, detail string, cause error) *Error {
	return &Error{Code: code, Path: path, Detail: detail, Cause: cause}
}

func AsError(err error) (*Error, bool) {
	var target *Error
	if errors.As(err, &target) {
		return target, true
	}
	return nil, false
}

func IsCode(err error, code ErrorCode) bool {
	e, ok := AsError(err)
	return ok && e.Code == code
}

// Diagnostic represents non-fatal findings such as E_LOSSY downgrade reports.
type Diagnostic struct {
	Code       ErrorCode `json:"code"`
	Path       string    `json:"path"`
	Original   string    `json:"original"`
	Resolution string    `json:"resolution"`
}

func (d Diagnostic) Error() string {
	return fmt.Sprintf("%s: %s: %s -> %s", d.Code, d.Path, d.Original, d.Resolution)
}
