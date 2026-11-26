package slikernel

import (
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"strings"
)

// Code is a stable, machine-readable business error code.
type Code string

const (
	CodeSchema              Code = "E_SCHEMA"
	CodeJSON                Code = "E_JSON"
	CodeAmbiguousField      Code = "E_AMBIGUOUS_FIELD"
	CodeUnit                Code = "E_UNIT"
	CodeTimeBasis           Code = "E_TIME_BASIS"
	CodeDuplicateService    Code = "E_DUPLICATE_SERVICE"
	CodeDuplicateSLI        Code = "E_DUPLICATE_SLI"
	CodeRatioRange          Code = "E_RATIO_RANGE"
	CodeQuantileRange       Code = "E_QUANTILE_RANGE"
	CodeWindowLength        Code = "E_WINDOW_LENGTH"
	CodeThreadBudget        Code = "E_THREAD_BUDGET"
	CodeBufferBudget        Code = "E_BUFFER_BUDGET"
	CodeBackpressure        Code = "E_BACKPRESSURE"
	CodeCheckpointCorrupt   Code = "E_CHECKPOINT_CORRUPT"
	CodeCheckpointVersion   Code = "E_CHECKPOINT_VERSION"
	CodeNoCheckpoint        Code = "E_NO_CHECKPOINT"
	CodeInvalidInput        Code = "E_INVALID_INPUT"
	CodeObservation         Code = "E_OBSERVATION"
	CodeObservationSeq      Code = "E_OBSERVATION_SEQUENCE"
	CodeObservationRange    Code = "E_OBSERVATION_RANGE"
	CodeEdit                Code = "E_EDIT"
	CodeRule                Code = "E_RULE"
	CodeInternal            Code = "E_INTERNAL"
	CodeGracefulInterrupted Code = "E_GRACEFUL_INTERRUPTED"
)

// BizError carries a stable business code plus deterministic location context.
// Context is formatted with sorted keys so error strings do not depend on map
// iteration order.
type BizError struct {
	Code    Code           `json:"code"`
	Op      string         `json:"op"`
	Context map[string]any `json:"context,omitempty"`
	Err     error          `json:"-"`
}

func (e *BizError) Error() string {
	if e == nil {
		return "<nil>"
	}
	var suffix string
	if len(e.Context) > 0 {
		keys := make([]string, 0, len(e.Context))
		for key := range e.Context {
			keys = append(keys, key)
		}
		sort.Strings(keys)
		parts := make([]string, 0, len(keys))
		for _, key := range keys {
			parts = append(parts, fmt.Sprintf("%s=%v", key, e.Context[key]))
		}
		suffix = " [" + strings.Join(parts, ", ") + "]"
	}
	if e.Err != nil {
		return fmt.Sprintf("%s: %s%s: %v", e.Code, e.Op, suffix, e.Err)
	}
	return fmt.Sprintf("%s: %s%s", e.Code, e.Op, suffix)
}

func (e *BizError) Unwrap() error {
	return e.Err
}

func (e *BizError) MarshalJSON() ([]byte, error) {
	type alias BizError
	return json.Marshal((*alias)(e))
}

// ErrorCode extracts a stable business code from an error. Wrapped BizError
// values are recognized by errors.As.
func ErrorCode(err error) Code {
	if err == nil {
		return ""
	}
	var business *BizError
	if errors.As(err, &business) {
		return business.Code
	}
	return CodeInternal
}

func biz(code Code, op string, err error, context map[string]any) *BizError {
	return &BizError{Code: code, Op: op, Err: err, Context: context}
}

func ctx(key string, value any) map[string]any {
	return map[string]any{key: value}
}

func ctx2(key1 string, value1 any, key2 string, value2 any) map[string]any {
	return map[string]any{key1: value1, key2: value2}
}

func ctx3(key1 string, value1 any, key2 string, value2 any, key3 string, value3 any) map[string]any {
	return map[string]any{key1: value1, key2: value2, key3: value3}
}
