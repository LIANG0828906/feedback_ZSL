package xerr

import (
	"errors"
	"fmt"
)

const (
	ESchemaUnsupported          = "E_SCHEMA_UNSUPPORTED"
	ERequestInvalid             = "E_REQUEST_INVALID"
	EDocumentInvalid            = "E_DOCUMENT_INVALID"
	EFieldConflict              = "E_FIELD_CONFLICT"
	EActionConflict             = "E_ACTION_CONFLICT"
	EPortRangeInvalid           = "E_PORT_RANGE_INVALID"
	ETimeBaseInvalid            = "E_TIME_BASE_INVALID"
	ESelectorTreeInvalid        = "E_SELECTOR_TREE_INVALID"
	ESelectorOverlap            = "E_SELECTOR_OVERLAP"
	ESelectorCycle              = "E_SELECTOR_CYCLE"
	EWorkloadMissing            = "E_WORKLOAD_MISSING"
	EWorkloadExited             = "E_WORKLOAD_EXITED"
	EPolicyNotFound             = "E_POLICY_NOT_FOUND"
	EReplaceTargetActive        = "E_REPLACE_TARGET_ACTIVE"
	EPolicyLimitInvalid         = "E_POLICY_LIMIT_INVALID"
	EPolicyLimitZero            = "E_POLICY_LIMIT_ZERO"
	ENamespacePolicyLimit       = "E_NAMESPACE_POLICY_LIMIT"
	EPriorityRange              = "E_PRIORITY_RANGE"
	EBudgetWorkerInvalid        = "E_WORKER_BUDGET_INVALID"
	EBudgetBufferInvalid        = "E_BUFFER_BUDGET_INVALID"
	EWorkerBudgetExhausted      = "E_WORKER_BUDGET_EXHAUSTED"
	EBackpressure               = "E_BACKPRESSURE"
	ENamespaceBufferFull        = "E_NAMESPACE_BUFFER_FULL"
	ECheckpointOpen             = "E_CHECKPOINT_OPEN"
	ECheckpointCorrupt          = "E_CHECKPOINT_CORRUPT"
	ECheckpointTruncated        = "E_CHECKPOINT_TRUNCATED"
	ECheckpointVersionMismatch  = "E_CHECKPOINT_VERSION_MISMATCH"
	ECheckpointSequenceConflict = "E_CHECKPOINT_SEQUENCE_CONFLICT"
	EInjectionUnknown           = "E_INJECTION_UNKNOWN"
	EGateNoBaseline             = "E_GATE_DENIED_NO_BASELINE"
)

type Error struct {
	Code    string         `json:"code"`
	Message string         `json:"message"`
	Context map[string]any `json:"context,omitempty"`
	cause   error
}

func New(code, message string, context ...map[string]any) *Error {
	e := &Error{Code: code, Message: message}
	if len(context) > 0 {
		e.Context = context[0]
	}
	return e
}

func Wrap(code, message string, err error, context ...map[string]any) *Error {
	e := New(code, message, context...)
	e.cause = err
	return e
}

func (e *Error) Error() string {
	if e == nil {
		return ""
	}
	if e.cause != nil {
		return fmt.Sprintf("%s: %s: %v", e.Code, e.Message, e.cause)
	}
	return fmt.Sprintf("%s: %s", e.Code, e.Message)
}

func (e *Error) Unwrap() error { return e.cause }

func As(err error) (*Error, bool) {
	var target *Error
	if errors.As(err, &target) {
		return target, true
	}
	return nil, false
}

func CodeOf(err error) string {
	if e, ok := As(err); ok {
		return e.Code
	}
	return "E_INTERNAL"
}
