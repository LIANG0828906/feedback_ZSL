package registry

import (
	"errors"
	"fmt"
)

const (
	CodeSchemaUnknown          = "E_SCHEMA_UNKNOWN"
	CodeSchemaField            = "E_SCHEMA_FIELD"
	CodeSchemaLocation         = "E_SCHEMA_LOCATION"
	CodeSchemaVersionMismatch  = "E_SCHEMA_VERSION_MISMATCH"
	CodeDuplicateService       = "E_DUPLICATE_SERVICE"
	CodeDuplicateInstance      = "E_DUPLICATE_INSTANCE"
	CodeUnknownService         = "E_UNKNOWN_SERVICE"
	CodeUnknownInstance        = "E_UNKNOWN_INSTANCE"
	CodeInvalidDuration        = "E_INVALID_DURATION"
	CodeInvalidHeartbeat       = "E_INVALID_HEARTBEAT"
	CodeInvalidThreshold       = "E_INVALID_THRESHOLD"
	CodeThresholdAmbiguous     = "E_THRESHOLD_AMBIGUOUS"
	CodeLeaseUnitAmbiguous     = "E_LEASE_UNIT_AMBIGUOUS"
	CodeLeaseWatermarkConflict = "E_LEASE_WATERMARK_CONFLICT"
	CodeInvalidWeightScale     = "E_INVALID_WEIGHT_SCALE"
	CodeInvalidWeight          = "E_INVALID_WEIGHT"
	CodeInvalidHealth          = "E_INVALID_HEALTH"
	CodeInvalidEvent           = "E_INVALID_EVENT"
	CodeInvalidBudget          = "E_INVALID_BUDGET"
	CodeEventOrder             = "E_EVENT_ORDER"
	CodeEditCommittedHistory   = "E_EDIT_COMMITTED_HISTORY"
	CodeEditInvalid            = "E_EDIT_INVALID"
	CodeCheckpointTruncated    = "E_CHECKPOINT_TRUNCATED"
	CodeCheckpointCorrupt      = "E_CHECKPOINT_CORRUPT"
	CodeCheckpointVersion      = "E_CHECKPOINT_VERSION_MISMATCH"
	CodeCheckpointIO           = "E_CHECKPOINT_IO"
	CodeVerificationFailed     = "E_VERIFY_FAILED"
	CodeInternal               = "E_INTERNAL"

	CodeProbeInFlight        = "E_PROBE_IN_FLIGHT"
	CodeProbeMaxDelay        = "E_PROBE_MAX_DELAY"
	CodeProbeBufferFull      = "E_PROBE_BUFFER_FULL"
	CodeProbeNoInflight      = "E_PROBE_NO_INFLIGHT"
	CodeProbeUnknownTask     = "E_PROBE_UNKNOWN_TASK"
	CodeProbeQueued          = "E_PROBE_QUEUED_NOT_RUNNING"
	CodeExcludeNotRegistered = "E_EXCL_NOT_REGISTERED"
	CodeExcludeLeaseExpired  = "E_EXCL_LEASE_EXPIRED"
	CodeExcludeDeregistered  = "E_EXCL_DEREGISTERED"
	CodeExcludeUnhealthy     = "E_EXCL_UNHEALTHY"
	CodeExcludeHalfOpen      = "E_EXCL_HALF_OPEN"
	CodeExcludeInitial       = "E_EXCL_INITIAL"
	CodeHalfOpenDemoted      = "W_HALF_OPEN_DEMOTED"
	CodeInitialAdmitted      = "W_INITIAL_ADMITTED"
	CodeHealthyAdmitted      = "W_HEALTHY_ADMITTED"

	ConstraintGlobal   = "C_GLOBAL_CONCURRENCY"
	ConstraintNode     = "C_NODE_CONCURRENCY"
	ConstraintRack     = "C_RACK_CONCURRENCY"
	ConstraintInstance = "C_INSTANCE_BUFFER"
)

type ErrorContext struct {
	ServiceID  string `json:"service_id,omitempty"`
	InstanceID string `json:"instance_id,omitempty"`
	Tick       *int64 `json:"tick,omitempty"`
	AtMS       *int64 `json:"at_ms,omitempty"`
	Path       string `json:"path,omitempty"`
	Constraint string `json:"constraint,omitempty"`
	QueueSeq   *int64 `json:"queue_seq,omitempty"`
	FieldName  string `json:"field,omitempty"`
	Checkpoint string `json:"checkpoint,omitempty"`
}

type Error struct {
	Code    string       `json:"code"`
	Message string       `json:"message"`
	Context ErrorContext `json:"context"`
	Cause   error        `json:"-"`
}

func (e *Error) Error() string {
	if e == nil {
		return ""
	}
	return fmt.Sprintf("%s: %s", e.Code, e.Message)
}

func (e *Error) Unwrap() error {
	return e.Cause
}

func NewError(code, message string, context ErrorContext) *Error {
	return &Error{Code: code, Message: message, Context: context}
}

func WrapError(code, message string, context ErrorContext, cause error) *Error {
	return &Error{Code: code, Message: message, Context: context, Cause: cause}
}

func AsError(err error) (*Error, bool) {
	var target *Error
	if errors.As(err, &target) {
		return target, true
	}
	return nil, false
}

func ErrorCode(err error) string {
	if err == nil {
		return ""
	}
	if biz, ok := AsError(err); ok {
		return biz.Code
	}
	return CodeInternal
}
