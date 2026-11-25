package model

import (
	"errors"
	"fmt"
)

const (
	CodeSchemaUnsupported          = "CHAOS_E_SCHEMA_UNSUPPORTED"
	CodeFieldConflict              = "CHAOS_E_FIELD_CONFLICT"
	CodeFieldRequired              = "CHAOS_E_FIELD_REQUIRED"
	CodeFieldInvalid               = "CHAOS_E_FIELD_INVALID"
	CodeCycleDependency            = "CHAOS_E_CYCLE_DEPENDENCY"
	CodeTargetMissingService       = "CHAOS_E_TARGET_MISSING_SERVICE"
	CodeNegativeDelay              = "CHAOS_E_NEGATIVE_DELAY"
	CodeTimeoutBudgetZero          = "CHAOS_E_TIMEOUT_BUDGET_ZERO"
	CodeConcurrencyLimitOutOfRange = "CHAOS_E_CONCURRENCY_LIMIT_OUT_OF_RANGE"
	CodeCheckpointCorrupt          = "CHAOS_E_CHECKPOINT_CORRUPT"
	CodeCheckpointVersionMismatch  = "CHAOS_E_CHECKPOINT_VERSION_MISMATCH"
	CodeProjectVersionMismatch     = "CHAOS_E_PROJECT_VERSION_MISMATCH"
	CodeMutationTargetMissing      = "CHAOS_E_MUTATION_TARGET_MISSING"
	CodeInternal                   = "CHAOS_E_INTERNAL"
)

type Error struct {
	Code    string         `json:"code"`
	Field   string         `json:"field,omitempty"`
	Message string         `json:"message"`
	Context map[string]any `json:"context,omitempty"`
}

func (e *Error) Error() string {
	if e.Field != "" {
		return fmt.Sprintf("%s at %s: %s", e.Code, e.Field, e.Message)
	}
	return fmt.Sprintf("%s: %s", e.Code, e.Message)
}

func NewError(code, field, format string, args ...any) *Error {
	return &Error{Code: code, Field: field, Message: fmt.Sprintf(format, args...)}
}

func WithContext(err *Error, key string, value any) *Error {
	if err.Context == nil {
		err.Context = map[string]any{}
	}
	err.Context[key] = value
	return err
}

func AsError(err error) (*Error, bool) {
	var ce *Error
	if errors.As(err, &ce) {
		return ce, true
	}
	return nil, false
}

func AsErrorOrInternal(err error) *Error {
	if ce, ok := AsError(err); ok {
		return ce
	}
	return NewError(CodeInternal, "", "%v", err)
}
