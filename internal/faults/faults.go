// Package faults defines the stable business error codes used across the
// scheduling component. Every error that can escape a public entry point is a
// *Fault carrying a deterministic E_xxx code plus locatable context.
package faults

import "fmt"

// Code is a stable, wire-safe business error code. Its spelling is part of the
// component contract and must never be reused with a different meaning.
type Code string

const (
	// E_SCHEMA_VERSION the requested schema version is unknown.
	E_SCHEMA_VERSION Code = "E_SCHEMA_VERSION"
	// E_FIELD_CONFLICT two fields that must agree disagree and cannot be
	// disambiguated without guessing.
	E_FIELD_CONFLICT Code = "E_FIELD_CONFLICT"
	// E_MISSING_FIELD a required field is absent.
	E_MISSING_FIELD Code = "E_MISSING_FIELD"
	// E_WEIGHT_RANGE a weight is outside the declared per-scale range.
	E_WEIGHT_RANGE Code = "E_WEIGHT_RANGE"
	// E_RESOURCE_NEGATIVE a resource quantity is negative.
	E_RESOURCE_NEGATIVE Code = "E_RESOURCE_NEGATIVE"
	// E_RESOURCE_PARSE a resource string cannot be parsed.
	E_RESOURCE_PARSE Code = "E_RESOURCE_PARSE"
	// E_TOPO_KEY_CONFLICT a topology key carries incompatible semantics across
	// two schema versions.
	E_TOPO_KEY_CONFLICT Code = "E_TOPO_KEY_CONFLICT"
	// E_OVERFLOW a fixed-point operation overflows the declared bit width.
	E_OVERFLOW Code = "E_OVERFLOW"
	// E_CONCURRENCY_BUDGET the concurrency budget is out of its legal range.
	E_CONCURRENCY_BUDGET Code = "E_CONCURRENCY_BUDGET"
	// E_BUFFER_BUDGET the per-batch candidate buffer budget is out of range.
	E_BUFFER_BUDGET Code = "E_BUFFER_BUDGET"
	// E_BACKPRESSURE the in-flight budget is exhausted and work is rejected
	// rather than silently admitted past the limit.
	E_BACKPRESSURE Code = "E_BACKPRESSURE"
	// E_CHECKPOINT_TRUNCATED a checkpoint file is truncated or corrupt.
	E_CHECKPOINT_TRUNCATED Code = "E_CHECKPOINT_TRUNCATED"
	// E_STATE_VERSION_MISMATCH persisted state was written by another state
	// format version and must be rebuilt from scratch.
	E_STATE_VERSION_MISMATCH Code = "E_STATE_VERSION_MISMATCH"
	// E_DUPLICATE_ID an identifier is declared twice.
	E_DUPLICATE_ID Code = "E_DUPLICATE_ID"
	// E_UNKNOWN_REF a rule or reference points at a missing object.
	E_UNKNOWN_REF Code = "E_UNKNOWN_REF"
	// E_CAP_EXCEEDED a topology-domain co-location cap is violated.
	E_CAP_EXCEEDED Code = "E_CAP_EXCEEDED"
)

// Fault is the single structured error type produced by the component.
type Fault struct {
	Code    Code
	Message string
	// Context holds deterministic locatable key/value pairs (entity ids, field
	// names, observed values ...). It never contains secrets.
	Context map[string]string
	Cause   error
}

func (f *Fault) Error() string {
	s := string(f.Code) + ": " + f.Message
	for k, v := range f.Context {
		s += fmt.Sprintf(" [%s=%s]", k, v)
	}
	if f.Cause != nil {
		s += " cause=" + f.Cause.Error()
	}
	return s
}

func (f *Fault) Unwrap() error { return f.Cause }

// New builds a Fault. Context pairs are added as alternating key, value
// arguments and must therefore be even in count.
func New(code Code, msg string, kv ...string) *Fault {
	f := &Fault{Code: code, Message: msg, Context: map[string]string{}}
	for i := 0; i+1 < len(kv); i += 2 {
		f.Context[kv[i]] = kv[i+1]
	}
	return f
}

// Wrap annotates an existing error.
func Wrap(code Code, msg string, cause error, kv ...string) *Fault {
	f := New(code, msg, kv...)
	f.Cause = cause
	return f
}

// CodeOf returns the Fault code of err, or empty string if err is not a Fault.
func CodeOf(err error) Code {
	if f, ok := err.(*Fault); ok {
		return f.Code
	}
	return ""
}

// Is reports whether err carries the given code.
func Is(err error, code Code) bool { return CodeOf(err) == code }
