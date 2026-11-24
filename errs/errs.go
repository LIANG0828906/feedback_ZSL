// Package errs defines the stable error codes used by the reverse-imaging
// pipeline. Errors always carry the file and field path when those are known.
package errs

import (
	"errors"
	"fmt"
	"strings"
)

// Code is a stable, machine-readable pipeline error code.
type Code string

const (
	CodeVersion  Code = "E_VER"
	CodeLossy    Code = "E_LOSSY"
	CodeParam    Code = "E_PARA"
	CodeDecode   Code = "E_DECODE"
	CodeGradient Code = "E_GRAD"
	CodeRound    Code = "E_ROUND"
	CodeDiverge  Code = "E_DIVERGE"
	CodeDecodeIO Code = "E_IO"
	CodeCancel   Code = "E_CANCEL"
	CodeCheckpt  Code = "E_CKPT"
	CodeRender   Code = "E_RENDER"
	CodeConfig   Code = "E_CONFIG"
	CodeConv     Code = "E_CONV"
)

// Error is the common error envelope for all reported pipeline failures.
type Error struct {
	Code    Code
	File    string
	Field   string
	Message string
	Details any
}

func (e *Error) Error() string {
	parts := []string{string(e.Code)}
	if e.File != "" {
		parts = append(parts, "file="+e.File)
	}
	if e.Field != "" {
		parts = append(parts, "field="+e.Field)
	}
	msg := strings.Join(parts, " ")
	if e.Message != "" {
		msg += ": " + e.Message
	}
	return msg
}

// New creates a coded pipeline error.
func New(code Code, file, field, format string, args ...any) *Error {
	return &Error{
		Code:    code,
		File:    file,
		Field:   field,
		Message: fmt.Sprintf(format, args...),
	}
}

// Wrap adds a code and field path to an existing error.
func Wrap(err error, code Code, file, field, format string, args ...any) error {
	if err == nil {
		return nil
	}
	msg := fmt.Sprintf(format, args...)
	if msg != "" {
		msg += ": " + err.Error()
	} else {
		msg = err.Error()
	}
	return &Error{Code: code, File: file, Field: field, Message: msg}
}

// CodeOf returns the pipeline code for err, if present.
func CodeOf(err error) (Code, bool) {
	var e *Error
	if errors.As(err, &e) {
		return e.Code, true
	}
	return "", false
}

// As returns the pipeline error envelope for err, if present.
func As(err error) (*Error, bool) {
	var e *Error
	if errors.As(err, &e) {
		return e, true
	}
	return nil, false
}
