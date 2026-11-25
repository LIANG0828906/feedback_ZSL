package bizerr

import (
	"errors"
	"fmt"
)

// Stable business codes used by the workbench, loaders, runtime and checkpoint.
const (
	CodeSchemaVersion      = "E_SCHEMA_VERSION"
	CodeSchemaField        = "E_SCHEMA_FIELD"
	CodeSchemaUnknownField = "E_SCHEMA_UNKNOWN_FIELD"
	CodeTimeOrder          = "E_TIME_ORDER"
	CodeThresholdNegative  = "E_THRESHOLD_NEGATIVE"
	CodeTopologyCycle      = "E_TOPOLOGY_CYCLE"
	CodeScaleOutOfRange    = "E_SCALE_OUT_OF_RANGE"
	CodeFixedOverflow      = "E_FIXED_OVERFLOW"
	CodeBackpressureBuffer = "E_BACKPRESSURE_BUFFER"
	CodeBackpressureWorker = "E_BACKPRESSURE_WORKERS"
	CodeCheckpointVersion  = "E_CHECKPOINT_VERSION"
	CodeCheckpointCorrupt  = "E_CHECKPOINT_CORRUPT"
	CodeCheckpointTrunc    = "E_CHECKPOINT_TRUNCATED"
	CodeEditTarget         = "E_EDIT_TARGET"
	CodeNotFound           = "E_NOT_FOUND"
	CodeInvalidInput       = "E_INVALID_INPUT"
)

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
	if e.Path != "" {
		return fmt.Sprintf("%s at %s: %s", e.Code, e.Path, e.Message)
	}
	return fmt.Sprintf("%s: %s", e.Code, e.Message)
}

func (e *Error) Unwrap() error { return e.Cause }

func (e *Error) With(key string, value any) *Error {
	if e.Context == nil {
		e.Context = map[string]any{}
	}
	e.Context[key] = value
	return e
}

func CodeOf(err error) string {
	var be *Error
	if errors.As(err, &be) {
		return be.Code
	}
	return ""
}

func IsCode(err error, code string) bool {
	return CodeOf(err) == code
}
