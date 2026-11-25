package plancache

import (
	"errors"
	"fmt"
)

// Stable business codes. Tests and operators should depend on these strings,
// not on the human-readable messages.
const (
	CodeOK = "E_OK"

	CodeSchemaUnknownVersion  = "E_SCHEMA_UNKNOWN_VERSION"
	CodeSchemaFieldConflict   = "E_SCHEMA_FIELD_CONFLICT"
	CodeSchemaMissingField    = "E_SCHEMA_MISSING_FIELD"
	CodeNodeTreeMalformed     = "E_NODE_TREE_MALFORMED"
	CodeParamIndexOutOfRange  = "E_PARAM_INDEX_OUT_OF_RANGE"
	CodeCapacityBytesZero     = "E_CAPACITY_BYTES_ZERO"
	CodeBudgetWorkersInvalid  = "E_BUDGET_WORKERS_INVALID"
	CodeBudgetBufferInvalid   = "E_BUDGET_BUFFER_INVALID"
	CodeBackpressureBuffer    = "E_BACKPRESSURE_BUFFER_FULL"
	CodeBackpressureWorkers   = "E_BACKPRESSURE_WORKERS_EXHAUSTED"
	CodeReferenceCycle        = "E_REFERENCE_CYCLE"
	CodeReferenceDangling     = "E_REFERENCE_DANGLING"
	CodeReferenceEvicted      = "E_REFERENCE_TO_EVICTED_PLAN"
	CodeFingerprintConflict   = "E_FINGERPRINT_CONFLICT"
	CodeCheckpointTruncated   = "E_CHECKPOINT_TRUNCATED"
	CodeCheckpointVersion     = "E_CHECKPOINT_VERSION_MISMATCH"
	CodeCheckpointCorrupt     = "E_CHECKPOINT_CORRUPT"
	CodeRequestNotFound       = "E_REQUEST_NOT_FOUND"
	CodeSourceProjectMutation = "E_SOURCE_PROJECT_MUTATION"
)

// BusinessError is the only error type returned for deterministic, reportable
// input/runtime failures. Path uses dotted/JSON-pointer-like segments.
type BusinessError struct {
	Code    string         `json:"code"`
	Path    string         `json:"path"`
	Message string         `json:"message"`
	Context map[string]any `json:"context,omitempty"`
}

func (e *BusinessError) Error() string {
	if e.Path == "" {
		return fmt.Sprintf("%s: %s", e.Code, e.Message)
	}
	return fmt.Sprintf("%s at %s: %s", e.Code, e.Path, e.Message)
}

func (e *BusinessError) Unwrap() error { return nil }

func newError(code, path, format string, args ...any) *BusinessError {
	return &BusinessError{Code: code, Path: path, Message: fmt.Sprintf(format, args...)}
}

func withContext(err *BusinessError, kv ...any) *BusinessError {
	if len(kv)%2 != 0 {
		panic("context arguments must be key/value pairs")
	}
	if err.Context == nil {
		err.Context = map[string]any{}
	}
	for i := 0; i < len(kv); i += 2 {
		err.Context[fmt.Sprint(kv[i])] = kv[i+1]
	}
	return err
}

// AsBusinessError extracts a deterministic code error when present.
func AsBusinessError(err error) (*BusinessError, bool) {
	var be *BusinessError
	if errors.As(err, &be) {
		return be, true
	}
	return nil, false
}
