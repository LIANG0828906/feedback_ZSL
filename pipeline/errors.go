package pipeline

import (
	"errors"
	"fmt"
)

// Stable business codes. Callers and the demo entry point may safely branch on
// these values; wrapped Go errors never replace them.
const (
	CodeSchemaUnsupported         = "E_SCHEMA_UNSUPPORTED"
	CodeSchemaAmbiguous           = "E_SCHEMA_AMBIGUOUS"
	CodeTimeBasisMissing          = "E_TIME_BASIS_MISSING"
	CodeFeatureDuplicate          = "E_FEATURE_DUPLICATE"
	CodeFeatureWindowWidth        = "E_FEATURE_WINDOW_WIDTH"
	CodeFeatureStepZero           = "E_FEATURE_STEP_ZERO"
	CodeFeatureWidthBelowStep     = "E_FEATURE_WIDTH_BELOW_STEP"
	CodeFeatureUnknown            = "E_FEATURE_UNKNOWN"
	CodeAggWeightOutOfRange       = "E_AGG_WEIGHT_OUT_OF_RANGE"
	CodeAggFunctionUnknown        = "E_AGG_FUNCTION_UNKNOWN"
	CodeEventUnknownFeature       = "E_EVENT_UNKNOWN_FEATURE"
	CodeEventSequenceInvalid      = "E_EVENT_SEQUENCE_INVALID"
	CodeWatermarkMissing          = "E_WATERMARK_MISSING"
	CodeWatermarkRegression       = "E_WATERMARK_REGRESSION"
	CodeWatermarkBeforeCommitted  = "E_WATERMARK_BEFORE_COMMITTED"
	CodeInvalidWorkers            = "E_INVALID_WORKERS"
	CodeThreadBudgetExceeded      = "E_THREAD_BUDGET_EXCEEDED"
	CodeInvalidWindowBuffer       = "E_INVALID_WINDOW_BUFFER"
	CodeBufferBudgetExceeded      = "E_BUFFER_BUDGET_EXCEEDED"
	CodeBackpressureWindowBuffer  = "E_BACKPRESSURE_WINDOW_BUFFER"
	CodeCheckpointTruncated       = "E_CHECKPOINT_TRUNCATED"
	CodeCheckpointVersionMismatch = "E_CHECKPOINT_VERSION_MISMATCH"
	CodeCheckpointStore           = "E_CHECKPOINT_STORE"
	CodeInterrupted               = "E_INTERRUPTED"
	CodeEditTargetInvalid         = "E_EDIT_TARGET_INVALID"
	CodeResultMismatch            = "E_RESULT_MISMATCH"
)

// BusinessError is a stable, contextual error suitable for CLI/API output.
type BusinessError struct {
	Code    string            `json:"code"`
	Message string            `json:"message"`
	Context map[string]string `json:"context,omitempty"`
	Err     error             `json:"-"`
}

func (e *BusinessError) Error() string {
	if len(e.Context) == 0 {
		return e.Code + ": " + e.Message
	}
	return fmt.Sprintf("%s: %s (%v)", e.Code, e.Message, e.Context)
}

func (e *BusinessError) Unwrap() error { return e.Err }

// NewBusinessError constructs a stable business error.
func NewBusinessError(code, format string, args ...any) *BusinessError {
	return &BusinessError{Code: code, Message: fmt.Sprintf(format, args...), Context: map[string]string{}}
}

// WithContext adds a locator to a business error.
func (e *BusinessError) WithContext(key, value string) *BusinessError {
	if e.Context == nil {
		e.Context = map[string]string{}
	}
	e.Context[key] = value
	return e
}

// Wrap attaches an underlying implementation error without replacing Code.
func (e *BusinessError) Wrap(err error) *BusinessError {
	e.Err = err
	return e
}

// AsBusinessError extracts a stable code from standard or wrapped errors.
func AsBusinessError(err error) (*BusinessError, bool) {
	var be *BusinessError
	if errors.As(err, &be) {
		return be, true
	}
	return nil, false
}

// ErrorCode returns the stable code or an implementation fallback.
func ErrorCode(err error) string {
	if be, ok := AsBusinessError(err); ok {
		return be.Code
	}
	return "E_INTERNAL"
}
