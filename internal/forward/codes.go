package forward

import (
	"errors"
	"fmt"
)

// Stable business codes used by loader, engine, scheduler, checkpoint and workbench.
const (
	CodeOK = "E_OK"

	CodeSchemaUnknown       = "E_SCHEMA_UNKNOWN"
	CodeSchemaParse         = "E_SCHEMA_PARSE"
	CodeFieldType           = "E_FIELD_TYPE"
	CodeFieldRequired       = "E_FIELD_REQUIRED"
	CodeFieldConflict       = "E_FIELD_CONFLICT"
	CodeFieldOutOfRange     = "E_FIELD_OUT_OF_RANGE"
	CodePeerNotFound        = "E_PEER_NOT_FOUND"
	CodeTunnelNotFound      = "E_TUNNEL_NOT_FOUND"
	CodeRuleNotFound        = "E_RULE_NOT_FOUND"
	CodeActionInvalid       = "E_ACTION_INVALID"
	CodeRuleOverlapConflict = "E_RULE_OVERLAP_CONFLICT"

	CodeMTUTooSmall       = "E_MTU_TOO_SMALL"
	CodePathMTUTooSmall   = "E_PMTU_TOO_SMALL"
	CodeKeepaliveNegative = "E_KEEPALIVE_NEGATIVE"
	CodeStateSkip         = "E_STATE_SKIP_INTERMEDIATE"
	CodeEventUnknown      = "E_EVENT_UNKNOWN"

	CodeFragmentTooMany  = "E_FRAG_TOO_MANY"
	CodeFragmentOverflow = "E_FRAG_OVERFLOW"
	CodeFragmentWidth    = "E_FRAG_WIDTH_UNSUPPORTED"

	CodeBudgetWorkers          = "E_BUDGET_WORKERS_OUT_OF_RANGE"
	CodeBudgetBuffer           = "E_BUDGET_BUFFER_OUT_OF_RANGE"
	CodeBackpressureBufferFull = "E_BACKPRESSURE_TUNNEL_BUFFER_FULL"
	CodeBackpressureWorkers    = "E_BACKPRESSURE_WORKER_BUDGET_EXHAUSTED"

	CodeCheckpointOpen            = "E_CHECKPOINT_OPEN"
	CodeCheckpointWrite           = "E_CHECKPOINT_WRITE"
	CodeCheckpointRead            = "E_CHECKPOINT_READ"
	CodeCheckpointTruncated       = "E_CHECKPOINT_TRUNCATED"
	CodeCheckpointVersionMismatch = "E_CHECKPOINT_VERSION_MISMATCH"

	CodeEditInvalid = "E_EDIT_INVALID"
	CodeInternal    = "E_INTERNAL"
)

// BizError is the only error shape exposed at diagnostic/API boundaries.
type BizError struct {
	Code    string         `json:"code"`
	Path    string         `json:"path,omitempty"`
	Message string         `json:"message"`
	Context map[string]any `json:"context,omitempty"`
}

func (e *BizError) Error() string {
	if e == nil {
		return CodeOK
	}
	if e.Path == "" {
		return fmt.Sprintf("%s: %s", e.Code, e.Message)
	}
	return fmt.Sprintf("%s at %s: %s", e.Code, e.Path, e.Message)
}

func (e *BizError) WithContext(key string, value any) *BizError {
	if e.Context == nil {
		e.Context = map[string]any{}
	}
	e.Context[key] = value
	return e
}

func NewBizError(code, path, format string, args ...any) *BizError {
	return &BizError{Code: code, Path: path, Message: fmt.Sprintf(format, args...)}
}

func AsBizError(err error) (*BizError, bool) {
	var biz *BizError
	if errors.As(err, &biz) {
		return biz, true
	}
	return nil, false
}

func AsBizErrorOrInternal(err error) *BizError {
	if biz, ok := AsBizError(err); ok {
		return biz
	}
	return WrapBiz(CodeInternal, "", err, "internal error")
}

func WrapBiz(code, path string, err error, message string) *BizError {
	biz := NewBizError(code, path, "%s: %v", message, err)
	return biz
}
