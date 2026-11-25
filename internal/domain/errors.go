package domain

import (
	"fmt"
	"sort"
	"strings"
)

const (
	ESchemaVersion        = "E_SCHEMA_VERSION"
	ESchemaUnknownField   = "E_SCHEMA_UNKNOWN_FIELD"
	ESchemaInvalid        = "E_SCHEMA_INVALID"
	ESchemaRequired       = "E_SCHEMA_REQUIRED"
	ESchemaMixedUnits     = "E_SCHEMA_MIXED_UNITS"
	ENegativeQuota        = "E_NEGATIVE_QUOTA"
	EReserveExceedsQuota  = "E_RESERVE_EXCEEDS_QUOTA"
	EReserveExceedsNode   = "E_RESERVE_EXCEEDS_NODE"
	EInvalidWeight        = "E_INVALID_WEIGHT"
	EInvalidCapacity      = "E_INVALID_CAPACITY"
	ETenantUnknown        = "E_TENANT_UNKNOWN"
	ENodeUnknown          = "E_NODE_UNKNOWN"
	ENodeNotAllowed       = "E_NODE_NOT_ALLOWED"
	EEventOrder           = "E_EVENT_ORDER"
	EEventInvalid         = "E_EVENT_INVALID"
	EFaultDomainUnknown   = "E_FAULT_DOMAIN_UNKNOWN"
	EFaultDomainCycle     = "E_FAULT_DOMAIN_CYCLE"
	EFixedPointOverflow   = "E_FIXED_POINT_OVERFLOW"
	EFixedPointSaturation = "E_FIXED_POINT_SATURATION"
	EFixedPointPrecision  = "E_FIXED_POINT_PRECISION"
	ELedgerInvariant      = "E_LEDGER_INVARIANT"
	EReleaseExceedsUsed   = "E_RELEASE_EXCEEDS_USED"
	ENodeCapacityExceeded = "E_NODE_CAPACITY_EXCEEDED"
	EWeightBelowUsed      = "E_WEIGHT_BELOW_USED"
	EMigrationNoTarget    = "E_MIGRATION_NO_TARGET"
	EMigrationConflict    = "E_MIGRATION_CONFLICT"
	EBackpressureBudget   = "E_BACKPRESSURE_BUDGET"
	EBackpressureQueue    = "E_BACKPRESSURE_QUEUE"
	ECheckpointTruncated  = "E_CHECKPOINT_TRUNCATED"
	ECheckpointVersion    = "E_CHECKPOINT_VERSION"
	ECheckpointCorrupt    = "E_CHECKPOINT_CORRUPT"
	ECheckpointIO         = "E_CHECKPOINT_IO"
	EEditInvalid          = "E_EDIT_INVALID"
	EEditWouldBreakState  = "E_EDIT_WOULD_BREAK_STATE"
	EInjectionSource      = "E_INJECTION_SOURCE"
	EInternalInvariant    = "E_INTERNAL_INVARIANT"
)

// BizError is the stable, machine-readable error shape used across all layers.
// Path identifies the exact input field or state location involved.
type BizError struct {
	Code    string         `json:"code"`
	Path    string         `json:"path,omitempty"`
	Message string         `json:"message"`
	Details map[string]any `json:"details,omitempty"`
}

func (e *BizError) Error() string {
	if e.Path == "" {
		return fmt.Sprintf("%s: %s", e.Code, e.Message)
	}
	return fmt.Sprintf("%s at %s: %s", e.Code, e.Path, e.Message)
}

func NewBizError(code, path, format string, args ...any) *BizError {
	return &BizError{Code: code, Path: path, Message: fmt.Sprintf(format, args...)}
}

func WithDetail(err *BizError, key string, value any) *BizError {
	if err.Details == nil {
		err.Details = make(map[string]any)
	}
	err.Details[key] = value
	return err
}

func CodeOf(err error) string {
	if err == nil {
		return ""
	}
	if be, ok := err.(*BizError); ok {
		return be.Code
	}
	return EInternalInvariant
}

func IsCode(err error, code string) bool {
	return CodeOf(err) == code
}

func sortedKeys[V any](m map[string]V) []string {
	keys := make([]string, 0, len(m))
	for key := range m {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	return keys
}

func JoinStrings(values []string) string {
	cp := append([]string(nil), values...)
	sort.Strings(cp)
	return strings.Join(cp, ",")
}
