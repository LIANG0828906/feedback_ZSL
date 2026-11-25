package errs

import (
	"errors"
	"fmt"
)

const (
	ESchemaUnknown           = "E_SCHEMA_UNKNOWN"
	ESchemaCombo             = "E_SCHEMA_COMBO_UNRECOGNIZED"
	EValidation              = "E_VALIDATION"
	EPredicateMalformed      = "E_PREDICATE_MALFORMED"
	EDurationNegative        = "E_DURATION_NEGATIVE"
	EFixedOverflow           = "E_FIXED_OVERFLOW"
	EBudgetWorkersOutOfRange = "E_BUDGET_WORKERS_OOR"
	EBudgetBufferOutOfRange  = "E_BUDGET_BUFFER_OOR"
	EBackpressure            = "E_EVAL_BACKPRESSURE"
	EReceiverNotFound        = "E_RECEIVER_NOT_FOUND"
	EEditInvalid             = "E_EDIT_INVALID"
	ECheckpoint              = "E_CHECKPOINT"
	ECheckpointCorrupt       = "E_CHECKPOINT_CORRUPT"
	ECheckpointVersion       = "E_CHECKPOINT_VERSION_MISMATCH"
	EInterrupted             = "E_INTERRUPTED"
	EIO                      = "E_IO"
)

type Error struct {
	Code    string `json:"code"`
	Path    string `json:"path,omitempty"`
	Message string `json:"message"`
	Cause   error  `json:"-"`
}

func (e *Error) Error() string {
	if e.Path != "" {
		return fmt.Sprintf("%s at %s: %s", e.Code, e.Path, e.Message)
	}
	return fmt.Sprintf("%s: %s", e.Code, e.Message)
}

func (e *Error) Unwrap() error { return e.Cause }

func New(code, path, format string, args ...any) *Error {
	return &Error{Code: code, Path: path, Message: fmt.Sprintf(format, args...)}
}

func Wrap(code, path string, cause error, format string, args ...any) *Error {
	return &Error{Code: code, Path: path, Message: fmt.Sprintf(format, args...), Cause: cause}
}

func IsCode(err error, code string) bool {
	var coded *Error
	if errors.As(err, &coded) {
		return coded.Code == code
	}
	return false
}

func CodeOf(err error) string {
	var coded *Error
	if errors.As(err, &coded) {
		return coded.Code
	}
	return ""
}
