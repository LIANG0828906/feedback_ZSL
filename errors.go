package fogkernel

import (
	"errors"
	"fmt"
)

const (
	CodeSchemaVersion      = "E_SCHEMA_VERSION"
	CodeSchemaCombination  = "E_SCHEMA_COMBINATION"
	CodeSchemaField        = "E_SCHEMA_FIELD"
	CodeDensityShape       = "E_DENSITY_SHAPE"
	CodeDensityRange       = "E_DENSITY_RANGE"
	CodeCoeffNegative      = "E_COEFF_NEGATIVE"
	CodeFixedOverflow      = "E_FIXED_OVERFLOW"
	CodeFixedSaturated     = "E_FIXED_SATURATED"
	CodeFixedPrecision     = "E_FIXED_PRECISION"
	CodeAuditTolerance     = "E_AUDIT_TOLERANCE"
	CodeEditTarget         = "E_EDIT_TARGET"
	CodeBudgetWorkers      = "E_BUDGET_WORKERS"
	CodeBudgetBuffer       = "E_BUDGET_BUFFER"
	CodeBackpressure       = "E_BACKPRESSURE"
	CodeCheckpointOpen     = "E_CHECKPOINT_OPEN"
	CodeCheckpointWrite    = "E_CHECKPOINT_WRITE"
	CodeCheckpointTrunc    = "E_CHECKPOINT_TRUNCATED"
	CodeCheckpointVersion  = "E_CHECKPOINT_VERSION"
	CodeCheckpointConflict = "E_CHECKPOINT_CONFLICT"
	CodeInjectionUnknown   = "E_INJECTION_UNKNOWN"
	CodeSignalShutdown     = "E_SIGNAL_SHUTDOWN"
)

// BizError is the stable, locatable error envelope used by the domain, CLI and
// HTTP workbench. Callers can rely on Code rather than matching prose.
type BizError struct {
	Code    string         `json:"code"`
	Message string         `json:"message"`
	Path    string         `json:"path,omitempty"`
	Context map[string]any `json:"context,omitempty"`
}

func (e *BizError) Error() string {
	if e == nil {
		return ""
	}
	if e.Path != "" {
		return fmt.Sprintf("%s at %s: %s", e.Code, e.Path, e.Message)
	}
	return fmt.Sprintf("%s: %s", e.Code, e.Message)
}

func (e *BizError) WithContext(kv ...any) *BizError {
	if e.Context == nil {
		e.Context = map[string]any{}
	}
	for i := 0; i+1 < len(kv); i += 2 {
		e.Context[fmt.Sprint(kv[i])] = kv[i+1]
	}
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

func bizOr(err error, code, path string) error {
	if err == nil {
		return nil
	}
	if biz, ok := AsBizError(err); ok {
		return biz
	}
	return NewBizError(code, path, "%v", err)
}
