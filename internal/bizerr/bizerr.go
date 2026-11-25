// Package bizerr defines stable, machine-readable business errors.
package bizerr

import (
	"errors"
	"fmt"
)

const (
	OK                    = "E_OK"
	SchemaUnsupported     = "E_SCHEMA_UNSUPPORTED"
	SchemaParse           = "E_SCHEMA_PARSE"
	SchemaConflict        = "E_SCHEMA_CONFLICT"
	FieldMapping          = "E_FIELD_MAPPING"
	WindowNegative        = "E_WINDOW_NEGATIVE"
	WindowOverCap         = "E_WINDOW_OVER_CAP"
	WeightRange           = "E_WEIGHT_RANGE"
	WeightZero            = "E_WEIGHT_ZERO"
	DependencyCycle       = "E_DEPENDENCY_CYCLE"
	FrameTooLarge         = "E_FRAME_TOO_LARGE"
	BudgetRange           = "E_BUDGET_RANGE"
	BufferFull            = "E_BUFFER_FULL"
	ThreadBudgetExhausted = "E_THREAD_BUDGET_EXHAUSTED"
	CheckpointCorrupt     = "E_CHECKPOINT_CORRUPT"
	CheckpointVersion     = "E_CHECKPOINT_VERSION"
	IncrementalMismatch   = "E_INCREMENTAL_MISMATCH"
	FixedOverflow         = "E_FIXED_OVERFLOW"
	NotFound              = "E_NOT_FOUND"
	Internal              = "E_INTERNAL"
)

// Error is the only error type expected at component/demo boundaries.
type Error struct {
	Code    string         `json:"code"`
	Message string         `json:"message"`
	Path    string         `json:"path,omitempty"`
	Context map[string]any `json:"context,omitempty"`
	Cause   error          `json:"-"`
}

func New(code, path, format string, args ...any) *Error {
	return &Error{Code: code, Path: path, Message: fmt.Sprintf(format, args...)}
}

func Wrap(code, path string, err error, format string, args ...any) *Error {
	return &Error{Code: code, Path: path, Message: fmt.Sprintf(format, args...), Cause: err}
}

func (e *Error) Error() string {
	if e.Path == "" {
		return fmt.Sprintf("%s: %s", e.Code, e.Message)
	}
	return fmt.Sprintf("%s at %s: %s", e.Code, e.Path, e.Message)
}

func (e *Error) Unwrap() error { return e.Cause }

func (e *Error) With(k string, v any) *Error {
	if e.Context == nil {
		e.Context = map[string]any{}
	}
	e.Context[k] = v
	return e
}

// WithContext is a fluent alias used by low-level packages.
func (e *Error) WithContext(k string, v any) *Error {
	return e.With(k, v)
}

// CodeOf returns a stable E_xxx code. Unknown errors become E_INTERNAL.
func CodeOf(err error) string {
	var be *Error
	if errors.As(err, &be) {
		return be.Code
	}
	return Internal
}

// AsError converts an arbitrary error to *Error when possible.
func AsError(err error) (*Error, bool) {
	var be *Error
	ok := errors.As(err, &be)
	return be, ok
}
