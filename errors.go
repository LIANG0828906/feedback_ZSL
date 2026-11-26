package downsample

import (
	"errors"
	"fmt"
)

// Stable business codes. They are part of the local demo and integration API.
const (
	CodeOK                        = "E_OK"
	CodeSchemaUnsupportedVersion  = "E_SCHEMA_VERSION_UNSUPPORTED"
	CodeSchemaUnknownField        = "E_SCHEMA_UNRECOGNIZED_FIELD"
	CodeSchemaFieldCombination    = "E_SCHEMA_FIELD_COMBINATION_INVALID"
	CodeSchemaInvalidJSON         = "E_SCHEMA_JSON_INVALID"
	CodeWindowWidthZero           = "E_WINDOW_WIDTH_ZERO"
	CodeRelativeOffsetRange       = "E_RELATIVE_OFFSET_OUT_OF_RANGE"
	CodeWeightScaleOutOfRange     = "E_WEIGHT_SCALE_OUT_OF_RANGE"
	CodeWeightOverflow            = "E_WEIGHT_FIXED_OVERFLOW"
	CodeFunctionCycle             = "E_FUNCTION_TABLE_CYCLE"
	CodeFunctionNotFound          = "E_FUNCTION_NOT_FOUND"
	CodeBudgetWorkersOutOfRange   = "E_BUDGET_WORKERS_OUT_OF_RANGE"
	CodeBudgetBufferOutOfRange    = "E_BUDGET_BUFFER_OUT_OF_RANGE"
	CodeBackpressureWorkers       = "E_BACKPRESSURE_WORKER_BUDGET_EXHAUSTED"
	CodeBackpressureBuffer        = "E_BACKPRESSURE_WINDOW_BUFFER_EXHAUSTED"
	CodeCheckpointOpen            = "E_CHECKPOINT_OPEN_FAILED"
	CodeCheckpointTruncated       = "E_CHECKPOINT_TRUNCATED"
	CodeCheckpointVersionMismatch = "E_CHECKPOINT_VERSION_MISMATCH"
	CodeCheckpointCommitOrder     = "E_CHECKPOINT_COMMIT_ORDER"
	CodeWindowNotFound            = "E_WINDOW_NOT_FOUND"
	CodeMetricNotFound            = "E_METRIC_NOT_FOUND"
	CodeConfigEditInvalid         = "E_CONFIG_EDIT_INVALID"
	CodeStreamInterrupted         = "E_STREAM_INTERRUPTED"
	CodeInjectionUnknown          = "E_INJECTION_CASE_UNKNOWN"
)

// Error is the coded error returned by all public entry points.
type Error struct {
	Code      string         `json:"code"`
	Version   string         `json:"schema_version,omitempty"`
	FieldPath string         `json:"field_path,omitempty"`
	Message   string         `json:"message"`
	Context   map[string]any `json:"context,omitempty"`
	cause     error
}

func (e *Error) Error() string {
	if e == nil {
		return CodeOK
	}
	if e.FieldPath != "" {
		return fmt.Sprintf("%s [%s] %s: %s", e.Code, e.Version, e.FieldPath, e.Message)
	}
	if e.Version != "" {
		return fmt.Sprintf("%s [%s]: %s", e.Code, e.Version, e.Message)
	}
	return fmt.Sprintf("%s: %s", e.Code, e.Message)
}

func (e *Error) Unwrap() error { return e.cause }

func NewError(code, message string) *Error {
	return &Error{Code: code, Message: message}
}

func WrapError(code, message string, cause error) *Error {
	return &Error{Code: code, Message: message, cause: cause}
}

func (e *Error) WithVersion(version string) *Error {
	e.Version = version
	return e
}

func (e *Error) WithFieldPath(path string) *Error {
	e.FieldPath = path
	return e
}

func (e *Error) WithContext(key string, value any) *Error {
	if e.Context == nil {
		e.Context = map[string]any{}
	}
	e.Context[key] = value
	return e
}

func (e *Error) WithContextMap(ctx map[string]any) *Error {
	if e.Context == nil {
		e.Context = map[string]any{}
	}
	for k, v := range ctx {
		e.Context[k] = v
	}
	return e
}

// AsError returns the stable *Error form. Unknown errors are wrapped as an
// internal validation error while preserving the original cause.
func AsError(err error) *Error {
	if err == nil {
		return nil
	}
	var coded *Error
	if errors.As(err, &coded) {
		return coded
	}
	return WrapError("E_INTERNAL", "internal downsample kernel error", err)
}

func IsCode(err error, code string) bool {
	var coded *Error
	return errors.As(err, &coded) && coded.Code == code
}
