// Package bpderr defines stable business codes used by all pipeline edges.
package bpderr

import "fmt"

type Code string

const (
	ESchemaUnsupported     Code = "E_SCHEMA_UNSUPPORTED"
	ESchemaAmbiguous       Code = "E_SCHEMA_AMBIGUOUS"
	ESchemaInvalid         Code = "E_SCHEMA_INVALID"
	EBufferLimitZero       Code = "E_BUFFER_LIMIT_ZERO"
	EKeyRuleCycle          Code = "E_KEY_RULE_CYCLE"
	EKeyRuleAmbiguous      Code = "E_KEY_RULE_AMBIGUOUS"
	EBudgetOutOfRange      Code = "E_BUDGET_OUT_OF_RANGE"
	EAgentQueueFull        Code = "E_AGENT_QUEUE_FULL"
	EWorkerBudgetExhausted Code = "E_WORKER_BUDGET_EXHAUSTED"
	EWatermarkAheadAck     Code = "E_WATERMARK_AHEAD_ACK"
	EWatermarkBeforeAck    Code = "E_WATERMARK_BEFORE_ACK"
	ECheckpointTruncated   Code = "E_CHECKPOINT_TRUNCATED"
	ECheckpointCorrupt     Code = "E_CHECKPOINT_CORRUPT"
	ECheckpointVersion     Code = "E_CHECKPOINT_VERSION_MISMATCH"
	EInvariant             Code = "E_INVARIANT_VIOLATION"
	EInterrupted           Code = "E_SIGNAL_INTERRUPTED"
	EBatchTooLarge         Code = "E_BATCH_TOO_LARGE"
)

type Error struct {
	Code    Code           `json:"code"`
	Field   string         `json:"field,omitempty"`
	Message string         `json:"message"`
	Context map[string]any `json:"context,omitempty"`
	cause   error
}

func New(code Code, field, message string) *Error {
	return &Error{Code: code, Field: field, Message: message}
}

func Newf(code Code, field, format string, args ...any) *Error {
	return &Error{Code: code, Field: field, Message: fmt.Sprintf(format, args...)}
}

func Wrap(code Code, field string, err error, message string) *Error {
	return &Error{Code: code, Field: field, Message: message, cause: err}
}

func (e *Error) WithContext(key string, value any) *Error {
	if e.Context == nil {
		e.Context = map[string]any{}
	}
	e.Context[key] = value
	return e
}

func (e *Error) Error() string {
	if e.Field == "" {
		return fmt.Sprintf("%s: %s", e.Code, e.Message)
	}
	return fmt.Sprintf("%s [%s]: %s", e.Code, e.Field, e.Message)
}

func (e *Error) Unwrap() error { return e.cause }

func Attach(err error, key string, value any) error {
	if e, ok := As(err); ok {
		return e.WithContext(key, value)
	}
	return err
}

func As(err error) (*Error, bool) {
	if err == nil {
		return nil, false
	}
	if e, ok := err.(*Error); ok {
		return e, true
	}
	type unwrapper interface{ Unwrap() []error }
	if u, ok := err.(unwrapper); ok {
		for _, child := range u.Unwrap() {
			if e, ok := As(child); ok {
				return e, true
			}
		}
	}
	for err != nil {
		if e, ok := err.(*Error); ok {
			return e, true
		}
		u, ok := err.(interface{ Unwrap() error })
		if !ok {
			return nil, false
		}
		err = u.Unwrap()
	}
	return nil, false
}
