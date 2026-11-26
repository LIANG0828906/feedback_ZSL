package domain

import (
	"errors"
	"fmt"
	"net/http"
)

const (
	CodeBadRequest           = "E_BAD_REQUEST"
	CodeNotFound             = "E_NOT_FOUND"
	CodeSchemaConflict       = "E_SCHEMA_CONFLICT"
	CodeDuplicateShard       = "E_DUP_SHARD"
	CodeZeroLease            = "E_ZERO_LEASE"
	CodeBadBandwidthScale    = "E_BAD_BANDWIDTH_SCALE"
	CodeRetiredNode          = "E_RETIRED_NODE"
	CodeBackpressureThreads  = "E_BACKPRESSURE_THREADS"
	CodeBackpressureBuffer   = "E_BACKPRESSURE_BUFFER"
	CodeThreadBudgetInvalid  = "E_THREAD_BUDGET_INVALID"
	CodeBufferBudgetInvalid  = "E_BUFFER_BUDGET_INVALID"
	CodeCheckpointTruncated  = "E_CHECKPOINT_TRUNCATED"
	CodeCheckpointCorrupt    = "E_CHECKPOINT_CORRUPT"
	CodeSnapshotVersion      = "E_SNAPSHOT_VERSION"
	CodeCheckpointOrder      = "E_CHECKPOINT_ORDER"
	CodeLeaseConflict        = "E_LEASE_CONFLICT"
	CodeBudgetViolation      = "E_BUDGET_VIOLATION"
	CodeConcurrencyViolation = "E_CONCURRENCY_VIOLATION"
	CodeReadFreshUnavailable = "E_READ_FRESH_UNAVAILABLE"
	CodeWriteVersionConflict = "E_WRITE_VERSION_CONFLICT"
	CodeWriteGateClosed      = "E_WRITE_GATE_CLOSED"
	CodeEditInvalid          = "E_EDIT_INVALID"
	CodeIncrementalMismatch  = "E_INCREMENTAL_MISMATCH"
	CodeInvalidInterruption  = "E_INVALID_INTERRUPTION"
	CodeInvalidSnapshot      = "E_INVALID_SNAPSHOT"
	CodeBackpressureInvalid  = "E_BACKPRESSURE_INVALID"
	CodeUnsupportedSchema    = "E_UNSUPPORTED_SCHEMA"
	CodeSemanticHashMismatch = "E_SEMANTIC_HASH_MISMATCH"
	CodeInternal             = "E_INTERNAL"
)

type BizError struct {
	Code       string         `json:"code"`
	Op         string         `json:"op,omitempty"`
	Message    string         `json:"message"`
	Context    map[string]any `json:"context,omitempty"`
	HTTPStatus int            `json:"-"`
}

func (e *BizError) Error() string {
	if e.Op == "" {
		return fmt.Sprintf("%s: %s", e.Code, e.Message)
	}
	return fmt.Sprintf("%s: %s: %s", e.Code, e.Op, e.Message)
}

func NewBizError(code, op, message string, context map[string]any) *BizError {
	return &BizError{
		Code:       code,
		Op:         op,
		Message:    message,
		Context:    context,
		HTTPStatus: StatusForCode(code),
	}
}

func StatusForCode(code string) int {
	switch code {
	case CodeBadRequest, CodeSchemaConflict, CodeDuplicateShard, CodeZeroLease,
		CodeBadBandwidthScale, CodeRetiredNode, CodeThreadBudgetInvalid,
		CodeBufferBudgetInvalid, CodeEditInvalid, CodeInvalidInterruption,
		CodeInvalidSnapshot, CodeBackpressureInvalid, CodeUnsupportedSchema,
		CodeSnapshotVersion, CodeLeaseConflict, CodeReadFreshUnavailable,
		CodeWriteVersionConflict, CodeWriteGateClosed:
		return http.StatusBadRequest
	case CodeBackpressureThreads, CodeBackpressureBuffer:
		return http.StatusTooManyRequests
	case CodeNotFound:
		return http.StatusNotFound
	case CodeCheckpointTruncated, CodeCheckpointCorrupt, CodeCheckpointOrder:
		return http.StatusUnprocessableEntity
	case CodeInternal, CodeIncrementalMismatch, CodeSemanticHashMismatch:
		return http.StatusInternalServerError
	default:
		return http.StatusInternalServerError
	}
}

func AsBizError(err error) *BizError {
	if err == nil {
		return nil
	}
	var biz *BizError
	if errors.As(err, &biz) {
		return biz
	}
	return NewBizError(CodeInternal, "internal", err.Error(), nil)
}

type Issue struct {
	Code    string         `json:"code"`
	AtTick  int64          `json:"at_tick"`
	NodeID  string         `json:"node_id,omitempty"`
	ShardID string         `json:"shard_id,omitempty"`
	Detail  string         `json:"detail,omitempty"`
	Context map[string]any `json:"context,omitempty"`
}
