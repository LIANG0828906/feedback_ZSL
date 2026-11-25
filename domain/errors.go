package domain

import (
	"errors"
	"fmt"
)

// Code is a stable, machine-readable business result code.
type Code string

const (
	CodeSchemaVersion       Code = "E_SCHEMA_VERSION"
	CodeSchemaUnknown       Code = "E_SCHEMA_UNKNOWN_FIELD"
	CodeSchemaMissing       Code = "E_SCHEMA_MISSING_FIELD"
	CodeSchemaType          Code = "E_SCHEMA_TYPE"
	CodeSchemaConflict      Code = "E_SCHEMA_CONFLICT"
	CodeUnitConflict        Code = "E_UNIT_CONFLICT"
	CodeOverflow            Code = "E_OVERFLOW"
	CodeDigestInvalid       Code = "E_DIGEST_INVALID"
	CodeFamilyDuplicate     Code = "E_FAMILY_DUPLICATE"
	CodeBatchDuplicate      Code = "E_BATCH_DUPLICATE"
	CodeBatchUnknown        Code = "E_BATCH_UNKNOWN"
	CodeBatchDeleted        Code = "E_BATCH_DELETED"
	CodeBatchSuperseded     Code = "E_BATCH_SUPERSEDED"
	CodePatchRange          Code = "E_PATCH_RANGE"
	CodePatchResult         Code = "E_PATCH_RESULT"
	CodeCompactDrift        Code = "E_COMPACT_DRIFT"
	CodeDependencyCycle     Code = "E_DEP_CYCLE"
	CodeDependencyDeleted   Code = "E_DEP_DELETED"
	CodeDependencyConflict  Code = "E_DEP_CONFLICT"
	CodeCrossVersion        Code = "E_CROSS_VERSION_CONFLICT"
	CodeOperationOrder      Code = "E_OPERATION_ORDER"
	CodeOperationKind       Code = "E_OPERATION_KIND"
	CodeBudgetWorkers       Code = "E_BUDGET_WORKERS"
	CodeBudgetBuffer        Code = "E_BUDGET_BUFFER"
	CodeBackpressure        Code = "E_BACKPRESSURE"
	CodeCheckpointTruncated Code = "E_CHECKPOINT_TRUNCATED"
	CodeCheckpointVersion   Code = "E_CHECKPOINT_VERSION"
	CodeCheckpointCorrupt   Code = "E_CHECKPOINT_CORRUPT"
	CodeCheckpointWrite     Code = "E_CHECKPOINT_WRITE"
	CodeScanInvalid         Code = "E_SCAN_INVALID"
	CodeSourceImmutable     Code = "E_SOURCE_IMMUTABLE"
)

// Error carries a stable business code and a precise path/context. Callers can
// use errors.As without parsing human-readable messages.
type Error struct {
	Code    Code
	Context string
	Op      int
	BatchID int
	Message string
	Cause   error
}

func (e *Error) Error() string {
	if e == nil {
		return "<nil>"
	}
	parts := []string{string(e.Code)}
	if e.Context != "" {
		parts = append(parts, "at "+e.Context)
	}
	if e.Op != 0 {
		parts = append(parts, fmt.Sprintf("op=%d", e.Op))
	}
	if e.BatchID != 0 {
		parts = append(parts, fmt.Sprintf("batch=%d", e.BatchID))
	}
	if e.Message != "" {
		parts = append(parts, e.Message)
	}
	if e.Cause != nil {
		parts = append(parts, ": "+e.Cause.Error())
	}
	return join(parts, "; ")
}

func (e *Error) Unwrap() error { return e.Cause }

func join(parts []string, sep string) string {
	out := ""
	for i, p := range parts {
		if i > 0 {
			out += sep
		}
		out += p
	}
	return out
}

func NewError(code Code, context, message string) *Error {
	return &Error{Code: code, Context: context, Message: message}
}

func Errorf(code Code, context, format string, args ...any) *Error {
	return &Error{Code: code, Context: context, Message: fmt.Sprintf(format, args...)}
}

func WrapError(code Code, context string, err error) *Error {
	return &Error{Code: code, Context: context, Cause: err}
}

func CodeOf(err error) Code {
	var de *Error
	if errors.As(err, &de) {
		return de.Code
	}
	return ""
}

func HasCode(err error, code Code) bool {
	return CodeOf(err) == code
}
