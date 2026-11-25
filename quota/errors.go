package quota

import (
	"errors"
	"fmt"
	"strings"
)

const (
	CodeSchemaUnknown          = "E_SCHEMA_UNKNOWN"
	CodeSchemaField            = "E_SCHEMA_FIELD"
	CodeVersionMismatch        = "E_VERSION_MISMATCH"
	CodeNegativeQuota          = "E_NEGATIVE_QUOTA"
	CodeGuaranteeOverCapacity  = "E_GUARANTEE_OVER_CAPACITY"
	CodeQuotaRange             = "E_QUOTA_RANGE"
	CodeOverflow               = "E_OVERFLOW"
	CodeSaturated              = "E_SATURATED"
	CodeInvalidBudget          = "E_INVALID_BUDGET"
	CodeQueueLimit             = "E_QUEUE_LIMIT"
	CodeBackpressureWorker     = "E_BACKPRESSURE_WORKER"
	CodeBackpressureQueue      = "E_BACKPRESSURE_QUEUE"
	CodeInvalidEvent           = "E_INVALID_EVENT"
	CodeCheckpointTruncated    = "E_CHECKPOINT_TRUNCATED"
	CodeCheckpointVersion      = "E_CHECKPOINT_VERSION"
	CodeCheckpointCorrupt      = "E_CHECKPOINT_CORRUPT"
	CodeCheckpointMissing      = "E_CHECKPOINT_MISSING"
	CodeCheckpointInvariant    = "E_CHECKPOINT_INVARIANT"
	CodePrecisionToleranceMiss = "E_PRECISION_TOLERANCE"
	CodeEditTarget             = "E_EDIT_TARGET"
)

// BusinessError carries a stable machine-readable code plus enough context
// for callers to point at the rejected declaration or event.
type BusinessError struct {
	Code      string
	Op        string
	Path      string
	TenantID  string
	NodeID    string
	TimeMilli int64
	Detail    string
	Err       error
}

func (e *BusinessError) Error() string {
	var b strings.Builder
	b.WriteString(e.Code)
	if e.Op != "" {
		fmt.Fprintf(&b, " op=%s", e.Op)
	}
	if e.Path != "" {
		fmt.Fprintf(&b, " path=%s", e.Path)
	}
	if e.TenantID != "" {
		fmt.Fprintf(&b, " tenant=%s", e.TenantID)
	}
	if e.NodeID != "" {
		fmt.Fprintf(&b, " node=%s", e.NodeID)
	}
	if e.TimeMilli != 0 {
		fmt.Fprintf(&b, " time_ms=%d", e.TimeMilli)
	}
	if e.Detail != "" {
		fmt.Fprintf(&b, " detail=%q", e.Detail)
	}
	if e.Err != nil {
		fmt.Fprintf(&b, " cause=%q", e.Err.Error())
	}
	return b.String()
}

func (e *BusinessError) Unwrap() error { return e.Err }

func NewError(code, op, path, detail string) *BusinessError {
	return &BusinessError{Code: code, Op: op, Path: path, Detail: detail}
}

func ErrorCode(err error) string {
	var be *BusinessError
	if errors.As(err, &be) {
		return be.Code
	}
	return ""
}

func IsCode(err error, code string) bool { return ErrorCode(err) == code }
