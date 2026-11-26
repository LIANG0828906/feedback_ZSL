package kernel

import (
	"errors"
	"fmt"
	"strings"
)

// Stable business codes used by loaders, admission, recovery and diagnostics.
const (
	CodeOK = "E_OK"

	CodeInvalidJSON         = "E_INVALID_JSON"
	CodeUnknownField        = "E_UNKNOWN_FIELD"
	CodeSchemaVersion       = "E_SCHEMA_VERSION_UNSUPPORTED"
	CodeFieldConflict       = "E_FIELD_COMBINATION_CONFLICT"
	CodeNegativeQuota       = "E_NEGATIVE_QUOTA"
	CodeNegativeCapacity    = "E_NEGATIVE_CAPACITY"
	CodeNegativeAmount      = "E_NEGATIVE_AMOUNT"
	CodeGuaranteeOverQuota  = "E_GUARANTEE_EXCEEDS_QUOTA"
	CodeGuaranteeOvercommit = "E_GUARANTEE_OVERCOMMIT_NODE"
	CodeBurstPercentRange   = "E_BURST_PERCENT_OUT_OF_RANGE"
	CodeEventOrder          = "E_EVENT_TICK_OUT_OF_ORDER"
	CodeBurstConflict       = "E_BURST_EXPRESSION_CONFLICT"
	CodeDuplicateID         = "E_DUPLICATE_ID"
	CodeReferenceNotFound   = "E_REFERENCE_NOT_FOUND"
	CodeNodeAffinity        = "E_NODE_TENANT_AFFINITY"
	CodeCausalCycle         = "E_CAUSAL_REFERENCE_CYCLE"
	CodeTimeBasis           = "E_TIME_BASIS_INVALID"
	CodeBudgetRange         = "E_BUDGET_OUT_OF_RANGE"
	CodeWorkerBudget        = "E_WORKER_BUDGET_INVALID"
	CodeQueueBudget         = "E_QUEUE_BUDGET_INVALID"
	CodeOverflow            = "E_FIXED_POINT_OVERFLOW"
	CodeTolerance           = "E_TOLERANCE_EXCEEDED"
	CodeProjectMismatch     = "E_PROJECT_FINGERPRINT_MISMATCH"

	CodeHardCap           = "E_HARD_CAP_EXCEEDED"
	CodeNodeCapacity      = "E_NODE_CAPACITY_EXHAUSTED"
	CodeGuaranteeInvasion = "E_GUARANTEE_INVASION_BLOCKED"
	CodeQueueFull         = "E_BACKPRESSURE_QUEUE_EXHAUSTED"
	CodeWorkerBusy        = "E_BACKPRESSURE_WORKER_EXHAUSTED"
	CodeDecisionBudget    = "E_DECISION_BUDGET_EXCEEDED"

	CodeCheckpointTruncated = "E_CHECKPOINT_TRUNCATED"
	CodeCheckpointVersion   = "E_CHECKPOINT_VERSION_MISMATCH"
	CodeCheckpointCorrupt   = "E_CHECKPOINT_CORRUPT"
	CodeCheckpointOrder     = "E_CHECKPOINT_COMMIT_ORDER"
)

// Constraint IDs make fairness and guarantee violations mechanically locatable.
const (
	ConstraintHardCap           = "C-HARD-CAP-Q-PLUS-B"
	ConstraintNodeCapacity      = "C-NODE-CAPACITY"
	ConstraintGuaranteeFloor    = "C-GUARANTEE-FLOOR"
	ConstraintGuaranteeFirst    = "C-GUARANTEE-FIRST"
	ConstraintQueueLimit        = "C-PER-TENANT-QUEUE-LIMIT"
	ConstraintWorkerBudget      = "C-WORKER-BUDGET"
	ConstraintDecisionLatency   = "C-DECISION-LATENCY-BUDGET"
	ConstraintFixedTolerance    = "C-FIXED-POINT-TOLERANCE"
	ConstraintCheckpointVersion = "C-CHECKPOINT-FORMAT-VERSION"
)

// BizError is the only error shape expected across the public kernel API.
type BizError struct {
	Code         string         `json:"code"`
	Message      string         `json:"message"`
	Path         string         `json:"path,omitempty"`
	TenantID     ID             `json:"tenant_id,omitempty"`
	NodeID       ID             `json:"node_id,omitempty"`
	EventID      string         `json:"event_id,omitempty"`
	ConstraintID string         `json:"constraint_id,omitempty"`
	Details      map[string]any `json:"details,omitempty"`
}

func (e *BizError) Error() string {
	var b strings.Builder
	b.WriteString(e.Code)
	if e.Path != "" {
		b.WriteString(" at ")
		b.WriteString(e.Path)
	}
	if e.TenantID != "" {
		b.WriteString(" tenant=")
		b.WriteString(string(e.TenantID))
	}
	if e.NodeID != "" {
		b.WriteString(" node=")
		b.WriteString(string(e.NodeID))
	}
	if e.EventID != "" {
		b.WriteString(" event=")
		b.WriteString(e.EventID)
	}
	if e.ConstraintID != "" {
		b.WriteString(" constraint=")
		b.WriteString(e.ConstraintID)
	}
	if e.Message != "" {
		b.WriteString(": ")
		b.WriteString(e.Message)
	}
	return b.String()
}

func (e *BizError) WithPath(path string) *BizError {
	cp := *e
	cp.Path = path
	return &cp
}

func (e *BizError) WithTenant(id ID) *BizError {
	cp := *e
	cp.TenantID = id
	return &cp
}

func (e *BizError) WithNode(id ID) *BizError {
	cp := *e
	cp.NodeID = id
	return &cp
}

func (e *BizError) WithEvent(id string) *BizError {
	cp := *e
	cp.EventID = id
	return &cp
}

func (e *BizError) WithConstraint(id string) *BizError {
	cp := *e
	cp.ConstraintID = id
	return &cp
}

func (e *BizError) WithDetail(key string, value any) *BizError {
	cp := *e
	if cp.Details == nil {
		cp.Details = map[string]any{}
	}
	cp.Details[key] = value
	return &cp
}

func NewBizError(code, format string, args ...any) *BizError {
	return &BizError{Code: code, Message: fmt.Sprintf(format, args...)}
}

func AsBizError(err error) (*BizError, bool) {
	var be *BizError
	if errors.As(err, &be) {
		return be, true
	}
	return nil, false
}

func BizCode(err error) string {
	if be, ok := AsBizError(err); ok {
		return be.Code
	}
	return ""
}
