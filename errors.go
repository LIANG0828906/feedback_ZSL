package queryopt

import "fmt"

// Error is the stable business error returned by every acceptance path.
type Error struct {
	Code      string         `json:"code"`
	Message   string         `json:"message"`
	Context   map[string]any `json:"context,omitempty"`
	Cause     error          `json:"-"`
	Retryable bool           `json:"retryable"`
}

func (e *Error) Error() string {
	if len(e.Context) == 0 {
		return fmt.Sprintf("%s: %s", e.Code, e.Message)
	}
	return fmt.Sprintf("%s: %s (%v)", e.Code, e.Message, e.Context)
}

func (e *Error) Unwrap() error { return e.Cause }

func newError(code, format string, args ...any) *Error {
	return &Error{Code: code, Message: fmt.Sprintf(format, args...), Context: map[string]any{}}
}

func (e *Error) with(key string, value any) *Error {
	if e.Context == nil {
		e.Context = map[string]any{}
	}
	e.Context[key] = value
	return e
}

func (e *Error) wrap(err error) *Error {
	e.Cause = err
	return e
}

func AsError(err error) (*Error, bool) {
	if err == nil {
		return nil, false
	}
	if e, ok := err.(*Error); ok {
		return e, true
	}
	return nil, false
}

const (
	CodeOK = "E_OK"

	CodeInputInvalid          = "E_INPUT_INVALID"
	CodeSchemaVersion         = "E_SCHEMA_VERSION_UNSUPPORTED"
	CodeSnapshotMismatch      = "E_SCHEMA_VERSION_MISMATCH"
	CodeSnapshotTruncated     = "E_STORE_CHECKPOINT_TRUNCATED"
	CodeHistogramOrder        = "E_STAT_HISTOGRAM_ORDER"
	CodeSelectivityScale      = "E_STAT_SELECTIVITY_SCALE"
	CodeMissingTable          = "E_SCHEMA_TABLE_NOT_FOUND"
	CodeNegativeRows          = "E_STAT_ROW_COUNT_NEGATIVE"
	CodeBudgetInvalid         = "E_OPT_BUDGET_INVALID"
	CodeWorkerBackpressure    = "E_OPT_WORKER_BACKPRESSURE"
	CodeCandidateBackpressure = "E_OPT_CANDIDATE_BUFFER_EXHAUSTED"
	CodeDisconnectedPlan      = "E_PLAN_DISCONNECTED"
	CodePlaceholderUnbound    = "E_PRED_PLACEHOLDER_UNBOUND"
	CodeStoreFailed           = "E_STORE_WRITE_FAILED"
	CodeStoreCorrupt          = "E_STORE_CORRUPT"
)
