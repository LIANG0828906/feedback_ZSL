// Package bizerror defines stable machine-readable diagnostics used by all
// quota project loaders, gates, and checkpoint recovery paths.
package bizerror

import "fmt"

const (
	CodeSchemaUnsupported     = "E_SCHEMA_UNSUPPORTED"
	CodeSchemaVersionMismatch = "E_SCHEMA_VERSION_MISMATCH"
	CodeFieldRequired         = "E_FIELD_REQUIRED"
	CodeFieldInvalidType      = "E_FIELD_INVALID_TYPE"
	CodeFieldAmbiguous        = "E_FIELD_AMBIGUOUS"
	CodeValueOutOfRange       = "E_VALUE_OUT_OF_RANGE"
	CodeDuplicateID           = "E_DUPLICATE_ID"
	CodeReferenceMissing      = "E_REFERENCE_MISSING"
	CodeNotAllowed            = "E_TENANT_NOT_ALLOWED"
	CodeScaleOverflow         = "E_FIXED_SCALE_OVERFLOW"
	CodeScaleSaturated        = "E_FIXED_SCALE_SATURATED"
	CodeToleranceExceeded     = "E_TOLERANCE_EXCEEDED"
	CodeGateBudget            = "E_GATE_LATENCY_BUDGET"
	CodeQueueLimit            = "E_WAIT_QUEUE_LIMIT"
	CodeBackpressureWorkers   = "E_BACKPRESSURE_WORKER_BUDGET"
	CodeCheckpointVersion     = "E_CHECKPOINT_VERSION_MISMATCH"
	CodeCheckpointCorrupt     = "E_CHECKPOINT_CORRUPT"
	CodeCheckpointFuture      = "E_CHECKPOINT_FUTURE_TICK"
)

// Error is the stable business error. Code is safe for assertions and alerts;
// Path is a JSON-pointer-like location whenever the failure is document-bound.
type Error struct {
	Code       string `json:"code"`
	Path       string `json:"path,omitempty"`
	Constraint string `json:"constraint,omitempty"`
	Message    string `json:"message"`
	RetryTick  int64  `json:"retry_tick,omitempty"`
}

func (e *Error) Error() string {
	if e.Path != "" {
		return fmt.Sprintf("%s at %s: %s", e.Code, e.Path, e.Message)
	}
	return fmt.Sprintf("%s: %s", e.Code, e.Message)
}

func New(code, path, format string, args ...any) *Error {
	return &Error{Code: code, Path: path, Message: fmt.Sprintf(format, args...)}
}

func WithConstraint(err *Error, constraint string, retryTick int64) *Error {
	err.Constraint = constraint
	err.RetryTick = retryTick
	return err
}

// As attempts to unwrap err into *Error.
func As(err error) (*Error, bool) {
	for err != nil {
		if be, ok := err.(*Error); ok {
			return be, true
		}
		type unwrapper interface{ Unwrap() error }
		u, ok := err.(unwrapper)
		if !ok {
			return nil, false
		}
		err = u.Unwrap()
	}
	return nil, false
}
