package errcode

// Code is a stable business error code. Codes never change value between
// releases so checkpoints, logs and external automation can rely on them.
type Code string

const (
	OK Code = ""

	// Loader / schema problems.
	ESchemaVersion   Code = "E_SCHEMA_VERSION"   // unrecognised schema version
	ESchemaField     Code = "E_SCHEMA_FIELD"     // field combination invalid for the declared version
	EMissingField    Code = "E_MISSING_FIELD"    // required field absent
	EUnitUnknown     Code = "E_UNIT_UNKNOWN"     // unknown time/duration unit
	EDupFunction     Code = "E_DUP_FUNCTION"     // function id declared twice
	EDupRequest      Code = "E_DUP_REQUEST"      // request id declared twice
	EBadEnum         Code = "E_BAD_ENUM"         // unknown priority / tenant / class enum

	// Function declaration problems (injection chain).
	ENegConcurrency   Code = "E_NEG_CONCURRENCY"    // concurrency limit < 0
	ENegReserved      Code = "E_NEG_RESERVED"       // reserved concurrency < 0
	EReservedExceeds  Code = "E_RESERVED_EXCEEDS"   // reserved > total limit
	EPoolOutOfRange   Code = "E_POOL_OUT_OF_RANGE"  // warm pool size < 0 or > limit
	ENegColdTimeout   Code = "E_NEG_COLD_TIMEOUT"   // cold start timeout negative
	EBadBudget        Code = "E_BAD_BUDGET"         // budget ratio outside [0, 1] scale
	ETickOrder        Code = "E_TICK_ORDER"         // request tick out of order / negative
	EBadArrival       Code = "E_BAD_ARRIVAL"        // arrival tick outside horizon
	EUnknownFunction  Code = "E_UNKNOWN_FUNCTION"   // request references unknown function

	// Fixed-point arithmetic.
	EOverflow   Code = "E_OVERFLOW"    // declared bit-width integer overflow
	ESaturation Code = "E_SATURATION"  // value clamped at declared bound
	ETolerance  Code = "E_TOLERANCE"   // fixed-point error outside declared tolerance

	// Gate / simulation.
	EQueueFull       Code = "E_QUEUE_FULL"        // per-function request buffer exhausted
	EBackpressure    Code = "E_BACKPRESSURE"      // worker/buffer budget exhausted
	EBudgetExhausted Code = "E_BUDGET_EXHAUSTED"  // cold-start budget consumed
	EColdTimeout     Code = "E_COLD_TIMEOUT"      // cold creation exceeded timeout
	EWaitTimeout     Code = "E_WAIT_TIMEOUT"      // queued request past wait limit

	// Checkpoint.
	ECheckpointTruncated Code = "E_CHECKPOINT_TRUNCATED" // partial/corrupt checkpoint
	ECheckpointVersion   Code = "E_CHECKPOINT_VERSION"   // checkpoint schema version mismatch
	ECheckpointState     Code = "E_CHECKPOINT_STATE"     // persisted state inconsistent
)

// Error wraps a stable Code with locatable context. It implements error.
type Error struct {
	Code    Code
	Message string
	// Attrs carries locatable context (function id, request id, tick, field...).
	Attrs map[string]any
}

func (e *Error) Error() string {
	s := string(e.Code)
	if e.Message != "" {
		s += ": " + e.Message
	}
	for k, v := range e.Attrs {
		s += " " + k + "=" + toString(v)
	}
	return s
}

func toString(v any) string {
	switch t := v.(type) {
	case string:
		return t
	case int:
		return itoa(int64(t))
	case int64:
		return itoa(t)
	default:
		return "<?> "
	}
}

func itoa(v int64) string {
	if v == 0 {
		return "0"
	}
	neg := v < 0
	if neg {
		v = -v
	}
	var b [20]byte
	i := len(b)
	for v > 0 {
		i--
		b[i] = byte('0' + v%10)
		v /= 10
	}
	if neg {
		i--
		b[i] = '-'
	}
	return string(b[i:])
}

// New builds an *Error with the given stable code and locatable attributes.
func New(code Code, msg string, attrs ...any) *Error {
	e := &Error{Code: code, Message: msg, Attrs: map[string]any{}}
	for i := 0; i+1 < len(attrs); i += 2 {
		if key, ok := attrs[i].(string); ok {
			e.Attrs[key] = attrs[i+1]
		}
	}
	return e
}

// From extracts an *Error and its code from any error. Unknown errors map to
// the empty code, which callers should treat as an internal failure.
func From(err error) (Code, *Error) {
	if err == nil {
		return OK, nil
	}
	if e, ok := err.(*Error); ok {
		return e.Code, e
	}
	return Code("E_INTERNAL"), &Error{Code: "E_INTERNAL", Message: err.Error()}
}
