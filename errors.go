package hckernel

import (
	"errors"
	"fmt"
)

// ErrorCode is the stable business code used by CLI, tests and integrators.
type ErrorCode string

const (
	ErrOK ErrorCode = "E_OK"

	ErrUnknownSchemaVersion    ErrorCode = "E_SCHEMA_VERSION_UNSUPPORTED"
	ErrSchemaDecodeFailed      ErrorCode = "E_SCHEMA_DECODE_FAILED"
	ErrSchemaPayloadInvalid    ErrorCode = "E_SCHEMA_PAYLOAD_INVALID"
	ErrIntervalZero            ErrorCode = "E_PROBE_INTERVAL_ZERO"
	ErrIntervalInvalid         ErrorCode = "E_PROBE_INTERVAL_INVALID"
	ErrNegativeTimeout         ErrorCode = "E_NEGATIVE_TIMEOUT"
	ErrNegativeDuration        ErrorCode = "E_NEGATIVE_DURATION"
	ErrThresholdOutOfRange     ErrorCode = "E_THRESHOLD_OUT_OF_RANGE"
	ErrRackReferenceMissing    ErrorCode = "E_RACK_REFERENCE_MISSING"
	ErrInstanceDuplicate       ErrorCode = "E_INSTANCE_DUPLICATE"
	ErrInstanceNotFound        ErrorCode = "E_INSTANCE_NOT_FOUND"
	ErrRackNotFound            ErrorCode = "E_RACK_NOT_FOUND"
	ErrBudgetZero              ErrorCode = "E_BUDGET_ZERO"
	ErrWeightInvalid           ErrorCode = "E_WEIGHT_INVALID"
	ErrWorkerBudgetInvalid     ErrorCode = "E_WORKER_BUDGET_INVALID"
	ErrBackpressureBufferFull  ErrorCode = "E_BACKPRESSURE_BUFFER_FULL"
	ErrBackpressureWorker      ErrorCode = "E_BACKPRESSURE_WORKER_BUDGET"
	ErrBackpressureWorkersBusy ErrorCode = "E_BACKPRESSURE_WORKERS_BUSY"
	ErrProbeNotFound           ErrorCode = "E_PROBE_NOT_FOUND"
	ErrEditFieldUnsupported    ErrorCode = "E_EDIT_FIELD_UNSUPPORTED"
	ErrCheckpointTruncated     ErrorCode = "E_CHECKPOINT_TRUNCATED"
	ErrCheckpointCorrupt       ErrorCode = "E_CHECKPOINT_CORRUPT"
	ErrCheckpointVersionMatch  ErrorCode = "E_CHECKPOINT_VERSION_MISMATCH"
	ErrCheckpointWrite         ErrorCode = "E_CHECKPOINT_WRITE_FAILED"
	ErrInjectedCrash           ErrorCode = "E_INJECTED_CRASH"
	ErrDeferTimeout            ErrorCode = "E_DEFER_TIMEOUT"
)

// Error carries a stable code and locatable context. It never wraps or mutates
// the source project document.
type Error struct {
	Code       ErrorCode `json:"code"`
	Message    string    `json:"message"`
	Path       string    `json:"path,omitempty"`
	InstanceID string    `json:"instance_id,omitempty"`
	RackID     string    `json:"rack_id,omitempty"`
	EventID    string    `json:"event_id,omitempty"`
	Constraint string    `json:"constraint_id,omitempty"`
	Offset     int64     `json:"offset,omitempty"`
	Cause      error     `json:"-"`
}

func (e *Error) Error() string {
	where := ""
	if e.Path != "" {
		where += " path=" + e.Path
	}
	if e.InstanceID != "" {
		where += " instance=" + e.InstanceID
	}
	if e.RackID != "" {
		where += " rack=" + e.RackID
	}
	if e.EventID != "" {
		where += " event=" + e.EventID
	}
	if e.Constraint != "" {
		where += " constraint=" + e.Constraint
	}
	if e.Offset != 0 {
		where += fmt.Sprintf(" offset=%d", e.Offset)
	}
	if e.Cause != nil {
		return fmt.Sprintf("%s: %s%s: %v", e.Code, e.Message, where, e.Cause)
	}
	return fmt.Sprintf("%s: %s%s", e.Code, e.Message, where)
}

func (e *Error) Unwrap() error { return e.Cause }

func newError(code ErrorCode, msg string, opts ...func(*Error)) *Error {
	e := &Error{Code: code, Message: msg}
	for _, opt := range opts {
		opt(e)
	}
	return e
}

func withPath(path string) func(*Error) {
	return func(e *Error) { e.Path = path }
}

func withInstance(id string) func(*Error) {
	return func(e *Error) { e.InstanceID = id }
}

func withRack(id string) func(*Error) {
	return func(e *Error) { e.RackID = id }
}

func withEvent(id string) func(*Error) {
	return func(e *Error) { e.EventID = id }
}

func withOffset(offset int64) func(*Error) {
	return func(e *Error) { e.Offset = offset }
}

func withConstraint(id string) func(*Error) {
	return func(e *Error) { e.Constraint = id }
}

func withCause(err error) func(*Error) {
	return func(e *Error) { e.Cause = err }
}

// CodeOf extracts the stable business code. Unknown/internal errors return
// E_SCHEMA_PAYLOAD_INVALID only when they are *Error; otherwise an empty code.
func CodeOf(err error) ErrorCode {
	var he *Error
	if errors.As(err, &he) {
		return he.Code
	}
	return ""
}

// IsCode reports whether err contains the expected stable code.
func IsCode(err error, code ErrorCode) bool {
	return CodeOf(err) == code
}
