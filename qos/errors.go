package qos

import (
	"errors"
	"fmt"
)

// Code is a stable business error code exposed by the component.
type Code string

const (
	EJSON                Code = "E_JSON"
	ESCHEMA              Code = "E_SCHEMA"
	EFieldMissing        Code = "E_FIELD_MISSING"
	EFieldConflict       Code = "E_FIELD_CONFLICT"
	EFieldType           Code = "E_FIELD_TYPE"
	ERateNegative        Code = "E_RATE_NEGATIVE"
	ERateZero            Code = "E_RATE_ZERO"
	ERateScale           Code = "E_RATE_SCALE"
	ERateOverflow        Code = "E_RATE_OVERFLOW"
	EBucketTooSmall      Code = "E_BUCKET_TOO_SMALL"
	ECapacityNegative    Code = "E_CAPACITY_NEGATIVE"
	EWatermarkRange      Code = "E_WATERMARK_RANGE"
	EOverflow            Code = "E_OVERFLOW"
	ETimeAlignment       Code = "E_TIME_ALIGNMENT"
	ETimeNegative        Code = "E_TIME_NEGATIVE"
	EExpiryOrder         Code = "E_EXPIRY_ORDER"
	EDuplicatePacket     Code = "E_DUPLICATE_PACKET"
	EUnknownChannel      Code = "E_UNKNOWN_CHANNEL"
	EThreadRange         Code = "E_THREAD_RANGE"
	EQueueBudget         Code = "E_QUEUE_BUDGET"
	EWeightRange         Code = "E_WEIGHT_RANGE"
	EPolicy              Code = "E_POLICY"
	EDelayQueue          Code = "E_DELAY_QUEUE"
	ECheckpointTruncated Code = "E_CHECKPOINT_TRUNCATED"
	ECheckpointVersion   Code = "E_CHECKPOINT_VERSION"
	ECheckpointCorrupt   Code = "E_CHECKPOINT_CORRUPT"
	ECheckpointEmpty     Code = "E_CHECKPOINT_EMPTY"
	EEditTarget          Code = "E_EDIT_TARGET"
	EHorizon             Code = "E_HORIZON"
	EContextCanceled     Code = "E_CONTEXT_CANCELED"
	EDSCPRange           Code = "E_DSCP_RANGE"
	EPriorityRange       Code = "E_PRIORITY_RANGE"
	EInternal            Code = "E_INTERNAL"
)

// Error carries a stable business code plus a JSON-pointer-like location and
// machine-readable context. It never contains mutable project state.
type Error struct {
	Code    Code           `json:"code"`
	Path    string         `json:"path,omitempty"`
	Message string         `json:"message"`
	Context map[string]any `json:"context,omitempty"`
	Err     error          `json:"-"`
}

func (e *Error) Error() string {
	if e == nil {
		return ""
	}
	if e.Path != "" {
		return fmt.Sprintf("%s at %s: %s", e.Code, e.Path, e.Message)
	}
	return fmt.Sprintf("%s: %s", e.Code, e.Message)
}

func (e *Error) Unwrap() error { return e.Err }

func NewError(code Code, path, message string, context map[string]any) *Error {
	return &Error{Code: code, Path: path, Message: message, Context: context}
}

func wrapError(code Code, path string, err error, context map[string]any) *Error {
	if err == nil {
		return nil
	}
	var qe *Error
	if errors.As(err, &qe) {
		return qe
	}
	return &Error{Code: code, Path: path, Message: err.Error(), Context: context, Err: err}
}

func CodeOf(err error) Code {
	var qe *Error
	if errors.As(err, &qe) {
		return qe.Code
	}
	return ""
}

func ErrorContext(err error) map[string]any {
	var qe *Error
	if errors.As(err, &qe) {
		return qe.Context
	}
	return nil
}
