package dialoguecal

import "fmt"

// Code is the stable business error identifier used by loaders, scheduling,
// fixed-point arithmetic, and checkpoint recovery.
type Code string

const (
	CodeSchemaUnknown       Code = "E_SCHEMA_UNKNOWN"
	CodeSchemaConflict      Code = "E_SCHEMA_CONFLICT"
	CodeGainNegative        Code = "E_GAIN_NEGATIVE"
	CodeTargetLoudnessRange Code = "E_TARGET_LOUDNESS_RANGE"
	CodeFrameRateZero       Code = "E_FRAME_RATE_ZERO"
	CodeFrameRateRange      Code = "E_FRAME_RATE_RANGE"
	CodeBudgetWorkers       Code = "E_BUDGET_WORKERS"
	CodeBudgetBuffer        Code = "E_BUDGET_BUFFER"
	CodeBackpressure        Code = "E_BACKPRESSURE"
	CodePhonemeOrder        Code = "E_PHONEME_ORDER"
	CodePhonemeCoverageGap  Code = "E_PHONEME_GAP"
	CodePhonemeOverlap      Code = "E_PHONEME_OVERLAP"
	CodeFrameBounds         Code = "E_FRAME_BOUNDS"
	CodeFixedOverflow       Code = "E_FIXED_OVERFLOW"
	CodeSnapshotVersion     Code = "E_SNAPSHOT_VERSION"
	CodeCheckpointCorrupt   Code = "E_CHECKPOINT_CORRUPT"
	CodeCheckpointTruncated Code = "E_CHECKPOINT_TRUNCATED"
	CodeCheckpointVersion   Code = "E_CHECKPOINT_VERSION"
	CodeCheckpointOrder     Code = "E_CHECKPOINT_ORDER"
	CodeInvalidInjection    Code = "E_INVALID_INJECTION"
)

// BusinessError contains a stable code and locatable context. Path uses
// dot/index notation, for example segments[0].gain_permille.
type BusinessError struct {
	Code       Code           `json:"code"`
	Path       string         `json:"path,omitempty"`
	SegmentID  string         `json:"segment_id,omitempty"`
	PhonemeID  string         `json:"phoneme_id,omitempty"`
	FrameIndex int            `json:"frame_index,omitempty"`
	Detail     string         `json:"detail,omitempty"`
	Context    map[string]any `json:"context,omitempty"`
}

func (e *BusinessError) Error() string {
	location := e.Path
	if location == "" {
		location = e.SegmentID
	}
	if location == "" {
		location = fmt.Sprintf("frame=%d", e.FrameIndex)
	}
	if e.Detail != "" {
		return fmt.Sprintf("%s at %s: %s", e.Code, location, e.Detail)
	}
	return fmt.Sprintf("%s at %s", e.Code, location)
}

func newError(code Code, path string, format string, args ...any) *BusinessError {
	return &BusinessError{Code: code, Path: path, Detail: fmt.Sprintf(format, args...)}
}

// AsBusinessError returns the stable code when err is or wraps BusinessError.
func AsBusinessError(err error) (*BusinessError, bool) {
	for err != nil {
		if be, ok := err.(*BusinessError); ok {
			return be, true
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
