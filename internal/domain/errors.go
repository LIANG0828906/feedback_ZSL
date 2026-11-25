package domain

import (
	"errors"
	"fmt"
	"strings"
)

const (
	ESchemaUnknownFieldCombo   = "E_SCHEMA_UNKNOWN_FIELD_COMBO"
	ESchemaVersionUnsupported  = "E_SCHEMA_VERSION_UNSUPPORTED"
	ETimeBaseConflict          = "E_TIME_BASE_CONFLICT"
	EInvalidWindow             = "E_INVALID_WINDOW"
	EInvalidCapacity           = "E_INVALID_CAPACITY"
	ENegativeWatermark         = "E_INPUT_NEGATIVE_WATERMARK"
	EWatermarkOutOfRange       = "E_WATERMARK_OUT_OF_RANGE"
	EMigrationExceedsWindow    = "E_MIGRATION_EXCEEDS_WINDOW"
	ELoanStartAfterExpiry      = "E_LEASE_EXPIRY_BEFORE_START"
	ELoanTargetMaintenance     = "E_LEASE_TARGET_MAINTENANCE_NODE"
	ELoanDoubleBooked          = "E_LOAN_DOUBLE_BOOKED"
	ELoanBorrowLimitExceeded   = "E_LOAN_BORROW_LIMIT_EXCEEDED"
	EThreadBudgetInvalid       = "E_THREAD_BUDGET_INVALID"
	EQueueBudgetInvalid        = "E_QUEUE_BUDGET_INVALID"
	EBackpressureThread        = "E_BACKPRESSURE_THREAD_EXHAUSTED"
	EBackpressureQueue         = "E_BACKPRESSURE_QUEUE_FULL"
	ECheckpointTruncated       = "E_CHECKPOINT_TRUNCATED"
	EStateVersionMismatch      = "E_STATE_VERSION_MISMATCH"
	EInstanceDualPlacement     = "E_INSTANCE_DUAL_PLACEMENT"
	EGateLoansOutstanding      = "E_GATE_LOANS_OUTSTANDING"
	EGateInstancesOutstanding  = "E_GATE_INSTANCES_OUTSTANDING"
	EIntervalConflict          = "E_INTERVAL_CONFLICT"
	EInvalidFaultInjection     = "E_INVALID_FAULT_INJECTION"
	EConstraintCapacity        = "C-CAPACITY-HARD-LIMIT"
	EConstraintWatermark       = "C-WATERMARK-NONDISRUPTION"
	EConstraintWaveConcurrency = "C-WAVE-CONCURRENCY-LIMIT"
	EConstraintMoveDuration    = "C-MIGRATION-MAX-DURATION"
	EConstraintWindow          = "C-MAINTENANCE-WINDOW"
	EConstraintLoanExclusive   = "C-LEASE-CAPACITY-EXCLUSIVE"
	EConstraintLoanBorrowCap   = "C-LEASE-BORROWER-CAP"
	EConstraintReturnGate      = "C-RETURN-GATE"
)

type Error struct {
	Code         string         `json:"code"`
	Path         string         `json:"path"`
	Message      string         `json:"message"`
	ConstraintID string         `json:"constraint_id,omitempty"`
	Details      map[string]any `json:"details,omitempty"`
}

func (e *Error) Error() string {
	var b strings.Builder
	fmt.Fprintf(&b, "%s at %s: %s", e.Code, e.Path, e.Message)
	if e.ConstraintID != "" {
		fmt.Fprintf(&b, " [%s]", e.ConstraintID)
	}
	return b.String()
}

func NewError(code, path, format string, args ...any) *Error {
	return &Error{Code: code, Path: normalizePath(path), Message: fmt.Sprintf(format, args...)}
}

func ConstraintError(code, path, constraintID, format string, args ...any) *Error {
	err := NewError(code, path, format, args...)
	err.ConstraintID = constraintID
	return err
}

func WithDetails(err *Error, details map[string]any) *Error {
	if err == nil {
		return nil
	}
	if err.Details == nil {
		err.Details = map[string]any{}
	}
	for k, v := range details {
		err.Details[k] = v
	}
	return err
}

func AsError(err error) (*Error, bool) {
	var de *Error
	if errors.As(err, &de) {
		return de, true
	}
	return nil, false
}

func normalizePath(path string) string {
	if path == "" {
		return "$"
	}
	if path == "$" || strings.HasPrefix(path, "$.") || strings.HasPrefix(path, "$[") {
		return path
	}
	if strings.HasPrefix(path, "[") {
		return "$" + path
	}
	return "$." + strings.TrimPrefix(path, ".")
}

func JoinPath(base, next string) string {
	if next == "" {
		return normalizePath(base)
	}
	if base == "" || base == "$" {
		if strings.HasPrefix(next, "[") {
			return "$" + next
		}
		return normalizePath(next)
	}
	if strings.HasPrefix(next, "[") {
		return base + next
	}
	return base + "." + next
}

func IsCode(err error, code string) bool {
	de, ok := AsError(err)
	return ok && de.Code == code
}
