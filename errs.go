package cachebench

import (
	"errors"
	"fmt"
	"sort"
	"strings"
)

// ErrorCode is the stable business code used by all validation, routing,
// backpressure and checkpoint failure paths.
type ErrorCode string

const (
	CodeOK                  ErrorCode = "E_OK"
	CodeNegativeCapacity    ErrorCode = "E_NEGATIVE_CAPACITY"
	CodeCapacityOverflow    ErrorCode = "E_CAPACITY_OVERFLOW"
	CodeWeightScale         ErrorCode = "E_WEIGHT_SCALE_OUT_OF_RANGE"
	CodeBreakerOrder        ErrorCode = "E_BREAKER_THRESHOLD_ORDER"
	CodeNodeNotFound        ErrorCode = "E_NODE_NOT_FOUND"
	CodeNoEligibleNode      ErrorCode = "E_NO_ELIGIBLE_NODE"
	CodeWorkerBudget        ErrorCode = "E_WORKER_BUDGET_OUT_OF_RANGE"
	CodeBufferBudget        ErrorCode = "E_BUFFER_BUDGET_OUT_OF_RANGE"
	CodeBackpressureThread  ErrorCode = "E_BACKPRESSURE_THREAD_BUDGET"
	CodeBackpressureBuffer  ErrorCode = "E_BACKPRESSURE_BUFFER_FULL"
	CodeCheckpointTruncated ErrorCode = "E_CHECKPOINT_TRUNCATED"
	CodeCheckpointVersion   ErrorCode = "E_CHECKPOINT_VERSION_MISMATCH"
	CodeSchemaVersion       ErrorCode = "E_SCHEMA_VERSION_UNSUPPORTED"
	CodeSchemaField         ErrorCode = "E_SCHEMA_FIELD_INVALID"
	CodeFixedOverflow       ErrorCode = "E_FIXED_POINT_OVERFLOW"
	CodeSQLiteCorrupt       ErrorCode = "E_SQLITE_CORRUPT"
	CodeInvalidArgument     ErrorCode = "E_INVALID_ARGUMENT"
	CodeIncrementalConflict ErrorCode = "E_INCREMENTAL_EDIT_CONFLICT"
)

// DomainError carries a stable code plus locatable context. Context values are
// rendered with sorted keys so error text remains deterministic.
type DomainError struct {
	Code         ErrorCode
	Message      string
	Field        string
	NodeID       string
	ConstraintID string
	BasisID      string
	Tick         int64
	Context      map[string]string
}

func (e *DomainError) Error() string {
	var b strings.Builder
	b.WriteString(string(e.Code))
	if e.Message != "" {
		b.WriteString(": ")
		b.WriteString(e.Message)
	}
	loc := e.Location()
	if loc != "" {
		b.WriteString(" [")
		b.WriteString(loc)
		b.WriteString("]")
	}
	return b.String()
}

func (e *DomainError) Unwrap() error { return ErrDomain }

// ErrDomain allows errors.Is(err, ErrDomain) for all business errors.
var ErrDomain = errors.New("cachebench domain error")

func NewError(code ErrorCode, format string, args ...any) *DomainError {
	return &DomainError{Code: code, Message: fmt.Sprintf(format, args...)}
}

func (e *DomainError) WithField(field string) *DomainError {
	e.Field = field
	return e
}

func (e *DomainError) WithNode(nodeID string) *DomainError {
	e.NodeID = nodeID
	return e
}

func (e *DomainError) WithConstraint(constraintID string) *DomainError {
	e.ConstraintID = constraintID
	return e
}

func (e *DomainError) WithBasis(basisID string) *DomainError {
	e.BasisID = basisID
	return e
}

func (e *DomainError) WithTick(tick int64) *DomainError {
	e.Tick = tick
	return e
}

func (e *DomainError) WithContext(key, value string) *DomainError {
	if e.Context == nil {
		e.Context = map[string]string{}
	}
	e.Context[key] = value
	return e
}

func (e *DomainError) Location() string {
	parts := make([]string, 0, 6+len(e.Context))
	if e.Field != "" {
		parts = append(parts, "field="+e.Field)
	}
	if e.NodeID != "" {
		parts = append(parts, "node="+e.NodeID)
	}
	if e.ConstraintID != "" {
		parts = append(parts, "constraint="+e.ConstraintID)
	}
	if e.BasisID != "" {
		parts = append(parts, "basis="+e.BasisID)
	}
	if e.Tick != 0 {
		parts = append(parts, fmt.Sprintf("tick=%d", e.Tick))
	}
	keys := make([]string, 0, len(e.Context))
	for k := range e.Context {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, k := range keys {
		parts = append(parts, k+"="+e.Context[k])
	}
	return strings.Join(parts, ", ")
}

func AsDomainError(err error) (*DomainError, bool) {
	var de *DomainError
	if errors.As(err, &de) {
		return de, true
	}
	return nil, false
}

func CodeOf(err error) ErrorCode {
	if de, ok := AsDomainError(err); ok {
		return de.Code
	}
	return ""
}
