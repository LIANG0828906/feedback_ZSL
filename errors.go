package planopt

import "fmt"

// Stable business codes. They are part of the locally demonstrable kernel API.
const (
	CodeOK                        = "E_OK"
	CodeSchemaUnknown             = "E_SCHEMA_UNKNOWN"
	CodeFieldCombination          = "E_UNRECOGNIZED_FIELD_COMBINATION"
	CodeMalformedNodeTree         = "E_MALFORMED_NODE_TREE"
	CodePlaceholderOutOfRange     = "E_PLACEHOLDER_OUT_OF_RANGE"
	CodeScaleOutOfRange           = "E_SCALE_OUT_OF_RANGE"
	CodeCheckpointTruncated       = "E_CHECKPOINT_TRUNCATED"
	CodeCheckpointVersionMismatch = "E_CHECKPOINT_VERSION_MISMATCH"
	CodeBackpressureWorkers       = "E_BACKPRESSURE_WORKER_BUDGET"
	CodeBackpressureBuffer        = "E_BACKPRESSURE_BUFFER_LIMIT"
	CodeInvalidEdit               = "E_INVALID_INCREMENTAL_EDIT"
	CodeInvariant                 = "E_INTERNAL_INVARIANT"
)

// Error is the only error shape crossed by loader, planner, scheduler and
// checkpoint boundaries. Code values must remain stable for callers.
type Error struct {
	Code   string `json:"code"`
	Op     string `json:"op,omitempty"`
	Detail string `json:"detail,omitempty"`
}

func (e *Error) Error() string {
	if e == nil {
		return CodeOK
	}
	if e.Op != "" {
		return fmt.Sprintf("%s: %s: %s", e.Code, e.Op, e.Detail)
	}
	return fmt.Sprintf("%s: %s", e.Code, e.Detail)
}

func NewError(code, op, detail string) *Error {
	return &Error{Code: code, Op: op, Detail: detail}
}

func Errorf(code, op, format string, args ...any) *Error {
	return &Error{Code: code, Op: op, Detail: fmt.Sprintf(format, args...)}
}

// AsError converts errors created by this package. Unknown errors are wrapped
// as E_INTERNAL_INVARIANT rather than leaking an unclassified failure.
func AsError(err error) (*Error, bool) {
	if err == nil {
		return nil, false
	}
	if e, ok := err.(*Error); ok {
		return e, true
	}
	return &Error{Code: CodeInvariant, Detail: err.Error()}, false
}
