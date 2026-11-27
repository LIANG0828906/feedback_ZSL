package bizerr

import (
	"errors"
	"fmt"
	"sort"
)

// Stable business codes. Callers may safely branch on these values.
const (
	SchemaUnknown       = "E_SCHEMA_UNKNOWN"
	SchemaFieldCombo    = "E_SCHEMA_FIELD_COMBO"
	SchemaRequired      = "E_SCHEMA_REQUIRED"
	SchemaDuplicateID   = "E_SCHEMA_DUPLICATE_ID"
	ListenerOverlap     = "E_LISTENER_OVERLAP"
	ListenerMissing     = "E_LISTENER_MISSING"
	UpstreamMissing     = "E_UPSTREAM_MISSING"
	UpstreamUnhealthy   = "E_UPSTREAM_UNHEALTHY"
	RewriteEmpty        = "E_REWRITE_EMPTY"
	RewriteStrip        = "E_REWRITE_STRIP"
	WeightRange         = "E_WEIGHT_RANGE"
	InflightRange       = "E_INFLIGHT_RANGE"
	TimeoutOrder        = "E_TIMEOUT_ORDER"
	FixedOverflow       = "E_FIXED_OVERFLOW"
	FixedSaturation     = "E_FIXED_SATURATION"
	BackpressureBuffer  = "E_BACKPRESSURE_BUFFER"
	BackpressureWorkers = "E_BACKPRESSURE_WORKERS"
	CheckpointTruncated = "E_CHECKPOINT_TRUNCATED"
	CheckpointChecksum  = "E_CHECKPOINT_CHECKSUM"
	CheckpointOrder     = "E_CHECKPOINT_ORDER"
	CheckpointSequence  = "E_CHECKPOINT_SEQUENCE"
	CheckpointVersion   = "E_CHECKPOINT_VERSION_MISMATCH"
	SnapshotVersion     = "E_SNAPSHOT_VERSION_MISMATCH"
	InvalidRequest      = "E_INVALID_REQUEST"
	InvalidEdit         = "E_INVALID_EDIT"
	InvalidArgument     = "E_INVALID_ARGUMENT"
	IO                  = "E_IO"
	Internal            = "E_INTERNAL"
)

// Error is a stable, structured business error. Context is copied whenever it
// is extended, so shared values cannot be modified accidentally.
type Error struct {
	Code    string            `json:"code"`
	Message string            `json:"message"`
	Context map[string]string `json:"context,omitempty"`
	Cause   error             `json:"-"`
}

func New(code, message string) *Error {
	return &Error{Code: code, Message: message, Context: map[string]string{}}
}

func Wrap(code, message string, cause error) *Error {
	return &Error{Code: code, Message: message, Context: map[string]string{}, Cause: cause}
}

func (e *Error) With(key, value string) *Error {
	if e == nil {
		return nil
	}
	ctx := make(map[string]string, len(e.Context)+1)
	for k, v := range e.Context {
		ctx[k] = v
	}
	ctx[key] = value
	return &Error{Code: e.Code, Message: e.Message, Context: ctx, Cause: e.Cause}
}

func (e *Error) WithInt(key string, value int) *Error {
	return e.With(key, fmt.Sprintf("%d", value))
}

func (e *Error) WithInt64(key string, value int64) *Error {
	return e.With(key, fmt.Sprintf("%d", value))
}

func (e *Error) Error() string {
	if e == nil {
		return "<nil>"
	}
	keys := make([]string, 0, len(e.Context))
	for k := range e.Context {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	ctx := ""
	for _, k := range keys {
		if ctx != "" {
			ctx += ","
		}
		ctx += k + "=" + e.Context[k]
	}
	if ctx != "" {
		return fmt.Sprintf("%s: %s (%s)", e.Code, e.Message, ctx)
	}
	return fmt.Sprintf("%s: %s", e.Code, e.Message)
}

func (e *Error) Unwrap() error {
	return e.Cause
}

func Code(err error) string {
	var e *Error
	if errors.As(err, &e) {
		return e.Code
	}
	if err == nil {
		return ""
	}
	return Internal
}

func Context(err error) map[string]string {
	var e *Error
	if errors.As(err, &e) {
		ctx := make(map[string]string, len(e.Context))
		for k, v := range e.Context {
			ctx[k] = v
		}
		return ctx
	}
	return nil
}

// As returns the structured error if err wraps one.
func As(err error) (*Error, bool) {
	var e *Error
	if errors.As(err, &e) {
		return e, true
	}
	return nil, false
}
