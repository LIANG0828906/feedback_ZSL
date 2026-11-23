package project

import (
	"fmt"
)

const (
	CodeSchema   = "E_SCHEMA"
	CodeSize     = "E_SIZE"
	CodeWeight   = "E_WEIGHT"
	CodeDecode   = "E_DECODE"
	CodeSeam     = "E_SEAM"
	CodeDiff     = "E_DIFF"
	CodeBake     = "E_BAKE"
	CodeCache    = "E_CACHE"
	CodeCancel   = "E_CANCEL"
	CodeResource = "E_RESOURCE"
	CodeTruth    = "E_TRUTH"
)

// Error is the stable, machine-readable error shape used by the loader,
// baker, cache, CLI and HTTP diagnostic endpoints.
type Error struct {
	Code    string         `json:"code"`
	BlockID string         `json:"block,omitempty"`
	Field   string         `json:"field,omitempty"`
	Message string         `json:"message"`
	Details map[string]any `json:"details,omitempty"`
}

func (e *Error) Error() string {
	if e == nil {
		return "<nil>"
	}
	s := e.Code
	if e.BlockID != "" {
		s += " block=" + e.BlockID
	}
	if e.Field != "" {
		s += " field=" + e.Field
	}
	if e.Message != "" {
		s += ": " + e.Message
	}
	return s
}

func NewError(code, blockID, field, format string, args ...any) *Error {
	return &Error{
		Code:    code,
		BlockID: blockID,
		Field:   field,
		Message: fmt.Sprintf(format, args...),
	}
}

func (e *Error) WithDetail(key string, value any) *Error {
	if e.Details == nil {
		e.Details = make(map[string]any)
	}
	e.Details[key] = value
	return e
}

func CodeOf(err error) string {
	if e, ok := err.(*Error); ok {
		return e.Code
	}
	return ""
}

func AsError(err error) *Error {
	if e, ok := err.(*Error); ok {
		return e
	}
	return nil
}
