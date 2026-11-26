// Package codes defines stable business error codes shared by the domain,
// persistence layer, diagnostics workbench, and acceptance tests.
package codes

import "fmt"

const (
	OK = "OK"

	ESchemaUnsupported       = "E_SCHEMA_UNSUPPORTED"
	EVersionConflict         = "E_VERSION_CONFLICT"
	EFieldMissing            = "E_FIELD_MISSING"
	EFieldType               = "E_FIELD_TYPE"
	EFieldInvalid            = "E_FIELD_INVALID"
	ERegionDuplicate         = "E_REGION_DUPLICATE"
	ERegionNotFound          = "E_REGION_NOT_FOUND"
	ERuleOverlap             = "E_RULE_OVERLAP"
	EUpstreamReference       = "E_UPSTREAM_REFERENCE_INVALID"
	ETTLRange                = "E_TTL_OUT_OF_RANGE"
	ETTLOverflow             = "E_TTL_OVERFLOW"
	ETimeoutRange            = "E_TIMEOUT_OUT_OF_RANGE"
	EIntOverflow             = "E_INT_OVERFLOW"
	EBudgetRange             = "E_BUDGET_OUT_OF_RANGE"
	EHopsRange               = "E_HOPS_OUT_OF_RANGE"
	ERouteNoMatch            = "E_ROUTE_NO_MATCH"
	EAnswerNotDeclared       = "E_ANSWER_NOT_DECLARED"
	EInterrupted             = "E_INTERRUPTED"
	EUpstreamCycle           = "E_UPSTREAM_FORWARD_CYCLE"
	EUpstreamHopsExceeded    = "E_UPSTREAM_HOPS_EXCEEDED"
	EUpstreamTimeout         = "E_UPSTREAM_TIMEOUT"
	EBackpressureWorkers     = "E_BACKPRESSURE_WORKER_BUDGET"
	EEditInvalid             = "E_EDIT_INVALID"
	EInjectionUnknown        = "E_INJECTION_UNKNOWN"
	ECheckpointOpen          = "E_CHECKPOINT_OPEN"
	ECheckpointCorrupt       = "E_CHECKPOINT_CORRUPT"
	ECheckpointTruncated     = "E_CHECKPOINT_TRUNCATED"
	ECheckpointVersion       = "E_CHECKPOINT_VERSION_MISMATCH"
	ECheckpointSequence      = "E_CHECKPOINT_SEQUENCE_INVALID"
	ECheckpointNotConfigured = "E_CHECKPOINT_NOT_CONFIGURED"
)

// Error is the only error shape crossing package and HTTP boundaries.
// Code is stable and safe for programmatic assertions; Context contains
// locatable, non-secret diagnostic fields.
type Error struct {
	Code    string         `json:"code"`
	Message string         `json:"message"`
	Field   string         `json:"field,omitempty"`
	Context map[string]any `json:"context,omitempty"`
}

func (e *Error) Error() string {
	if e.Field != "" {
		return fmt.Sprintf("%s: %s: %s", e.Code, e.Field, e.Message)
	}
	return fmt.Sprintf("%s: %s", e.Code, e.Message)
}

func New(code, message string) *Error {
	return &Error{Code: code, Message: message}
}

func Newf(code, format string, args ...any) *Error {
	return &Error{Code: code, Message: fmt.Sprintf(format, args...)}
}

func Wrap(code, message string, err error) *Error {
	biz := New(code, message)
	if err != nil {
		biz.Context = map[string]any{"cause": err.Error()}
	}
	return biz
}

func (e *Error) WithField(field string) *Error {
	e.Field = field
	return e
}

func (e *Error) WithContext(key string, value any) *Error {
	if e.Context == nil {
		e.Context = map[string]any{}
	}
	e.Context[key] = value
	return e
}

// As attempts to extract a stable *Error from err.
func As(err error) (*Error, bool) {
	for err != nil {
		if biz, ok := err.(*Error); ok {
			return biz, true
		}
		type unwrapper interface{ Unwrap() error }
		u, ok := err.(unwrapper)
		if !ok {
			return nil, false
		}
		err = u.Unwrap()
	}
	return nil, false
}

// CodeOf returns a stable code for any error. Unknown errors become E_INTERNAL.
func CodeOf(err error) string {
	if err == nil {
		return OK
	}
	if biz, ok := As(err); ok {
		return biz.Code
	}
	return "E_INTERNAL"
}
