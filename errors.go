package orchestrator

import (
	"errors"
	"fmt"
)

// Code is a stable, machine-readable business error code. Callers may depend on
// these values; error messages are intentionally less stable.
type Code string

const (
	CodeInvalidJSON       Code = "E_INVALID_JSON"
	CodeUnsupportedSchema Code = "E_UNSUPPORTED_SCHEMA"
	CodeInvalidSchema     Code = "E_INVALID_SCHEMA"
	CodeDuplicateNode     Code = "E_DUPLICATE_NODE"
	CodeUnknownNode       Code = "E_UNKNOWN_NODE"
	CodeDuplicateWorkload Code = "E_DUPLICATE_WORKLOAD"
	CodeUnknownWorkload   Code = "E_UNKNOWN_WORKLOAD"
	CodeInvalidEvent      Code = "E_INVALID_EVENT"
	CodeTimeNotAligned    Code = "E_TIME_NOT_ALIGNED"
	CodeNumericOverflow   Code = "E_NUMERIC_OVERFLOW"
	CodeTimeoutOverflow   Code = "E_TIMEOUT_OVERFLOW"
	CodeDependencyCycle   Code = "E_DEPENDENCY_CYCLE"
	CodeBudgetExceeded    Code = "E_BUDGET_EXCEEDED"
	CodeBackpressure      Code = "E_BACKPRESSURE"
	CodeCheckpoint        Code = "E_CHECKPOINT"
	CodeInterrupted       Code = "E_INTERRUPTED"
	CodeSnapshotChanged   Code = "E_SNAPSHOT_CHANGED"
)

// BusinessError carries a stable code plus a human-readable location. It is
// deliberately not a sentinel error: errors.As can be used without relying on
// any particular wrapped error instance.
type BusinessError struct {
	Code    Code   `json:"code"`
	Message string `json:"message"`
	Path    string `json:"path,omitempty"`
}

func (e *BusinessError) Error() string {
	if e.Path == "" {
		return fmt.Sprintf("%s: %s", e.Code, e.Message)
	}
	return fmt.Sprintf("%s at %s: %s", e.Code, e.Path, e.Message)
}

func newError(code Code, path, format string, args ...any) error {
	return &BusinessError{Code: code, Path: path, Message: fmt.Sprintf(format, args...)}
}

// CodeOf extracts a business code from an error. Unknown errors return "".
func CodeOf(err error) Code {
	var business *BusinessError
	if errors.As(err, &business) {
		return business.Code
	}
	var coded interface{ BusinessCode() Code }
	if errors.As(err, &coded) {
		return coded.BusinessCode()
	}
	return ""
}

// CycleError is returned when the dependency relation is cyclic. Each cycle is
// a closed walk with the first node repeated at the end so it is directly
// locatable in logs and reports.
type CycleError struct {
	Code   Code       `json:"code"`
	Cycles [][]string `json:"cycles"`
}

func (e *CycleError) Error() string {
	return fmt.Sprintf("%s: dependency cycles: %v", e.Code, e.Cycles)
}

func (e *CycleError) BusinessCode() Code { return e.Code }

func cycleError(cycles [][]string) error {
	return &CycleError{Code: CodeDependencyCycle, Cycles: cycles}
}
