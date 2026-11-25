package errs

import (
	"errors"
	"fmt"
)

const (
	SchemaUnsupportedVersion  = "E_SCHEMA_VERSION_UNSUPPORTED"
	SchemaUnknownField        = "E_SCHEMA_UNKNOWN_FIELD"
	SchemaInvalidValue        = "E_SCHEMA_INVALID_VALUE"
	RateOutOfRange            = "E_RATE_OUT_OF_RANGE"
	TailConditionCycle        = "E_TAIL_CONDITION_CYCLE"
	AllocationSumExceeded     = "E_ALLOC_SUM_EXCEEDED"
	EdgeServiceMissing        = "E_EDGE_SERVICE_MISSING"
	RuntimeWorkerBudgetBad    = "E_RUNTIME_WORKER_BUDGET_INVALID"
	RuntimeBufferBudgetBad    = "E_RUNTIME_BUFFER_BUDGET_INVALID"
	BackpressureWorkers       = "E_BACKPRESSURE_WORKERS"
	BackpressureBuffer        = "E_BACKPRESSURE_BUFFER"
	CheckpointTruncated       = "E_CHECKPOINT_TRUNCATED"
	CheckpointVersionMismatch = "E_CHECKPOINT_VERSION_MISMATCH"
	CheckpointOpenFailed      = "E_CHECKPOINT_OPEN_FAILED"
	CheckpointWriteFailed     = "E_CHECKPOINT_WRITE_FAILED"
	CheckpointReadFailed      = "E_CHECKPOINT_READ_FAILED"
	OrphanSpan                = "E_ORPHAN_SPAN"
	InvariantViolation        = "E_INVARIANT_VIOLATION"
)

type Error struct {
	Code    string `json:"code"`
	Version string `json:"schema_version,omitempty"`
	Path    string `json:"path,omitempty"`
	Message string `json:"message"`
	Detail  string `json:"detail,omitempty"`
}

func (e *Error) Error() string {
	if e.Path != "" {
		return fmt.Sprintf("%s [%s] %s: %s", e.Code, e.Version, e.Path, e.Message)
	}
	if e.Version != "" {
		return fmt.Sprintf("%s [%s]: %s", e.Code, e.Version, e.Message)
	}
	return fmt.Sprintf("%s: %s", e.Code, e.Message)
}

func New(code, message string) *Error {
	return &Error{Code: code, Message: message}
}

func (e *Error) WithVersion(version string) *Error {
	cp := *e
	cp.Version = version
	return &cp
}

func (e *Error) AtPath(path string) *Error {
	cp := *e
	cp.Path = path
	return &cp
}

func (e *Error) WithDetail(detail string) *Error {
	cp := *e
	cp.Detail = detail
	return &cp
}

func Wrap(code, message string, err error) *Error {
	if err == nil {
		return nil
	}
	return &Error{Code: code, Message: message + ": " + err.Error()}
}

func As(err error) (*Error, bool) {
	var target *Error
	if errors.As(err, &target) {
		return target, true
	}
	return nil, false
}

func CodeOf(err error) string {
	if coded, ok := As(err); ok {
		return coded.Code
	}
	return ""
}
