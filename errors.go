package locklab

import (
	"errors"
	"fmt"
)

// ErrorCode is a stable, machine-readable business error code.
type ErrorCode string

const (
	EJSONDuplicateKey       ErrorCode = "E_JSON_DUPLICATE_KEY"
	EJSONInvalid            ErrorCode = "E_JSON_INVALID"
	ESchemaVersionMissing   ErrorCode = "E_SCHEMA_VERSION_MISSING"
	ESchemaUnsupported      ErrorCode = "E_SCHEMA_UNSUPPORTED"
	ESchemaAmbiguous        ErrorCode = "E_SCHEMA_AMBIGUOUS"
	ESchemaUnknownField     ErrorCode = "E_SCHEMA_UNKNOWN_FIELD"
	EFieldRequired          ErrorCode = "E_FIELD_REQUIRED"
	EUnitAmbiguous          ErrorCode = "E_UNIT_AMBIGUOUS"
	EDuplicateItem          ErrorCode = "E_DUPLICATE_ITEM"
	EDuplicateTransaction   ErrorCode = "E_DUPLICATE_TRANSACTION"
	EUnknownItem            ErrorCode = "E_UNKNOWN_ITEM"
	EUnknownTable           ErrorCode = "E_UNKNOWN_TABLE"
	ELockModeUnknown        ErrorCode = "E_LOCK_MODE_UNKNOWN"
	ELockModeCompatMismatch ErrorCode = "E_LOCK_MODE_COMPAT_MISMATCH"
	EGranularityInvalid     ErrorCode = "E_GRANULARITY_INVALID"
	EIsolationUnknown       ErrorCode = "E_ISOLATION_UNKNOWN"
	EOperationInvalid       ErrorCode = "E_OPERATION_INVALID"
	EOperationOrder         ErrorCode = "E_OPERATION_ORDER"
	EOperationAfterTerminal ErrorCode = "E_OPERATION_AFTER_TERMINAL"
	ETimeoutReversed        ErrorCode = "E_TIMEOUT_REVERSED"
	EBackpressureWorkers    ErrorCode = "E_BACKPRESSURE_WORKERS"
	EBackpressureMemory     ErrorCode = "E_BACKPRESSURE_MEMORY"
	EBudgetInvalid          ErrorCode = "E_BUDGET_INVALID"
	EWaitGraphDangling      ErrorCode = "E_WAITGRAPH_DANGLING"
	ECheckpointNotFound     ErrorCode = "E_CHECKPOINT_NOT_FOUND"
	ECheckpointTruncated    ErrorCode = "E_CHECKPOINT_TRUNCATED"
	ECheckpointVersion      ErrorCode = "E_CHECKPOINT_VERSION_MISMATCH"
	EProjectVersion         ErrorCode = "E_PROJECT_VERSION_MISMATCH"
	EImmutableSource        ErrorCode = "E_IMMUTABLE_SOURCE"
	EInvalidEdit            ErrorCode = "E_INVALID_EDIT"
	EInterrupted            ErrorCode = "E_INTERRUPTED"
	ETruthMismatch          ErrorCode = "E_TRUTH_MISMATCH"
	EInternal               ErrorCode = "E_INTERNAL"
)

// Error carries a stable code and a JSONPath-like location.
type Error struct {
	Code    ErrorCode `json:"code"`
	Path    string    `json:"path,omitempty"`
	Message string    `json:"message"`
	Cause   error     `json:"-"`
}

func (e *Error) Error() string {
	if e.Path != "" {
		return fmt.Sprintf("%s at %s: %s", e.Code, e.Path, e.Message)
	}
	return fmt.Sprintf("%s: %s", e.Code, e.Message)
}

func (e *Error) Unwrap() error { return e.Cause }

func NewError(code ErrorCode, path, format string, args ...any) *Error {
	return &Error{Code: code, Path: path, Message: fmt.Sprintf(format, args...)}
}

func WrapError(code ErrorCode, path string, cause error, format string, args ...any) *Error {
	return &Error{Code: code, Path: path, Message: fmt.Sprintf(format, args...), Cause: cause}
}

func ErrorCodeOf(err error) ErrorCode {
	var target *Error
	if errors.As(err, &target) {
		return target.Code
	}
	return ""
}

func IsErrorCode(err error, code ErrorCode) bool {
	return ErrorCodeOf(err) == code
}
