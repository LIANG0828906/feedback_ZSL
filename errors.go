package thermalcache

import (
	"errors"
	"fmt"
	"sort"
	"strings"
)

// Stable business codes. Codes are part of the integration contract and must
// not be replaced by their human-readable messages.
const (
	CodeSchemaUnsupported     = "E_SCHEMA_UNSUPPORTED"
	CodeSchemaInvalid         = "E_SCHEMA_INVALID"
	CodeSchemaUnknownField    = "E_SCHEMA_UNKNOWN_FIELD"
	CodeDuplicateObject       = "E_DUPLICATE_OBJECT"
	CodeDuplicateNode         = "E_DUPLICATE_NODE"
	CodeCapacityNegative      = "E_CAPACITY_NEGATIVE"
	CodeCapacityOverflow      = "E_CAPACITY_OVERFLOW"
	CodeBatchLimitZero        = "E_BATCH_LIMIT_ZERO"
	CodeMigrationTarget       = "E_MIGRATION_TARGET_CAPACITY"
	CodeMigrationSource       = "E_MIGRATION_SOURCE"
	CodeMigrationBackpressure = "E_MIGRATION_BACKPRESSURE"
	CodeThreadBudget          = "E_THREAD_BUDGET"
	CodeBufferBudget          = "E_BUFFER_BUDGET"
	CodeCheckpointNotFound    = "E_CHECKPOINT_NOT_FOUND"
	CodeCheckpointTruncated   = "E_CHECKPOINT_TRUNCATED"
	CodeCheckpointVersion     = "E_CHECKPOINT_VERSION_MISMATCH"
	CodeCheckpointProject     = "E_CHECKPOINT_PROJECT_MISMATCH"
	CodeCheckpointInvalid     = "E_CHECKPOINT_INVALID"
	CodeSQLiteUnavailable     = "E_SQLITE_UNAVAILABLE"
	CodeGateWrongLayer        = "E_GATE_WRONG_LAYER"
	CodeGateTargetNotVisible  = "E_GATE_TARGET_NOT_VISIBLE"
	CodeObjectNotFound        = "E_OBJECT_NOT_FOUND"
	CodeNodeNotFound          = "E_NODE_NOT_FOUND"
	CodeEditInvalid           = "E_EDIT_INVALID"
	CodeInternal              = "E_INTERNAL"
)

// BizError is the only error type intentionally exposed by domain operations.
// Context is sorted when formatted so logs are stable across runs.
type BizError struct {
	Code    string         `json:"code"`
	Message string         `json:"message"`
	Context map[string]any `json:"context,omitempty"`
}

func (e *BizError) Error() string {
	if e == nil {
		return ""
	}
	var b strings.Builder
	b.WriteString(e.Code)
	if e.Message != "" {
		b.WriteString(": ")
		b.WriteString(e.Message)
	}
	if len(e.Context) == 0 {
		return b.String()
	}
	keys := make([]string, 0, len(e.Context))
	for k := range e.Context {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	b.WriteString(" [")
	for i, k := range keys {
		if i > 0 {
			b.WriteString(", ")
		}
		fmt.Fprintf(&b, "%s=%v", k, e.Context[k])
	}
	b.WriteString("]")
	return b.String()
}

func biz(code, message string, context map[string]any) *BizError {
	return &BizError{Code: code, Message: message, Context: context}
}

// ErrorCode returns a stable business code for err, or an empty string when
// err is not a domain error.
func ErrorCode(err error) string {
	var target *BizError
	if errors.As(err, &target) {
		return target.Code
	}
	return ""
}

func IsCode(err error, code string) bool {
	return ErrorCode(err) == code
}
