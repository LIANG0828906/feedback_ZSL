package kernel

import (
	"errors"
	"fmt"
)

const (
	CodeSchemaUnsupportedVersion  = "E_SCHEMA_UNSUPPORTED_VERSION"
	CodeSchemaUnknownField        = "E_SCHEMA_UNKNOWN_FIELD"
	CodeSchemaInvalidCombination  = "E_SCHEMA_INVALID_COMBINATION"
	CodeDefinitionNegativeWidth   = "E_DEFINITION_NEGATIVE_WIDTH"
	CodeDefinitionZeroStep        = "E_DEFINITION_ZERO_STEP"
	CodeDefinitionInvalidGeometry = "E_DEFINITION_INVALID_GEOMETRY"
	CodeAggUnknownFunction        = "E_AGG_UNKNOWN_FUNCTION"
	CodeAggDuplicateFunction      = "E_AGG_DUPLICATE_FUNCTION"
	CodeAggWeightOutOfRange       = "E_AGG_WEIGHT_OUT_OF_RANGE"
	CodeBudgetWorkersOutOfRange   = "E_BUDGET_WORKERS_OUT_OF_RANGE"
	CodeBudgetBufferOutOfRange    = "E_BUDGET_BUFFER_OUT_OF_RANGE"
	CodeBackpressureThread        = "E_BACKPRESSURE_THREAD_EXHAUSTED"
	CodeBackpressureBuffer        = "E_BACKPRESSURE_BUFFER_FULL"
	CodeWatermarkBeforeCommitted  = "E_WATERMARK_BEFORE_COMMITTED_WINDOW"
	CodeEventUnknownFeature       = "E_EVENT_UNKNOWN_FEATURE"
	CodeEventInvalidTime          = "E_EVENT_INVALID_TIME"
	CodeCheckpointOpen            = "E_CHECKPOINT_OPEN"
	CodeCheckpointTruncated       = "E_CHECKPOINT_TRUNCATED"
	CodeCheckpointVersionMismatch = "E_CHECKPOINT_VERSION_MISMATCH"
	CodeCheckpointCorrupt         = "E_CHECKPOINT_CORRUPT"
	CodeCheckpointTx              = "E_CHECKPOINT_TX"
	CodeInvalidArgument           = "E_INVALID_ARGUMENT"
)

type Error struct {
	Code    string         `json:"code"`
	Message string         `json:"message"`
	Context map[string]any `json:"context,omitempty"`
}

func (e *Error) Error() string {
	if len(e.Context) == 0 {
		return fmt.Sprintf("%s: %s", e.Code, e.Message)
	}
	return fmt.Sprintf("%s: %s (%v)", e.Code, e.Message, e.Context)
}

func NewError(code, message string, context map[string]any) *Error {
	return &Error{Code: code, Message: message, Context: context}
}

func WrapError(code string, err error, context map[string]any) *Error {
	if err == nil {
		return nil
	}
	return &Error{Code: code, Message: err.Error(), Context: context}
}

func AsError(err error) (*Error, bool) {
	var ke *Error
	if errors.As(err, &ke) {
		return ke, true
	}
	return nil, false
}

func CodeOf(err error) string {
	if ke, ok := AsError(err); ok {
		return ke.Code
	}
	return ""
}
