package layercache

import "fmt"

// Error codes are part of the component contract. Do not renumber or reuse
// them for a different meaning.
const (
	CodeJSONInvalid               = "E_JSON_INVALID"
	CodeVersionMissing            = "E_VERSION_MISSING"
	CodeVersionUnknown            = "E_VERSION_UNKNOWN"
	CodeVersionMismatch           = "E_VERSION_MISMATCH"
	CodeFieldMissing              = "E_FIELD_MISSING"
	CodeFieldType                 = "E_FIELD_TYPE"
	CodeAmbiguousInput            = "E_AMBIGUOUS_INPUT"
	CodeDuplicateObject           = "E_DUPLICATE_OBJECT"
	CodeDuplicateLayer            = "E_DUPLICATE_LAYER"
	CodeDuplicateEventOrder       = "E_DUPLICATE_EVENT_ORDER"
	CodeDuplicateSample           = "E_DUPLICATE_SAMPLE"
	CodeNegativeCapacity          = "E_NEGATIVE_CAPACITY"
	CodeZeroBatchLimit            = "E_ZERO_BATCH_LIMIT"
	CodeMissingLayer              = "E_MISSING_LAYER"
	CodeMissingObject             = "E_MISSING_OBJECT"
	CodeObjectNotAllowed          = "E_OBJECT_NOT_ALLOWED"
	CodeCapacityExceeded          = "E_CAPACITY_EXCEEDED"
	CodeInvalidCount              = "E_INVALID_COUNT"
	CodeInvalidEvent              = "E_INVALID_EVENT"
	CodeEventOrder                = "E_EVENT_ORDER"
	CodeInvalidScale              = "E_INVALID_SCALE"
	CodeFixedOverflow             = "E_FIXED_OVERFLOW"
	CodeWatermarkMismatch         = "E_WATERMARK_MISMATCH"
	CodeThreadBudget              = "E_THREAD_BUDGET"
	CodeBufferBudget              = "E_BUFFER_BUDGET"
	CodeBackpressureThreads       = "E_BACKPRESSURE_THREADS"
	CodeBackpressureBuffer        = "E_BACKPRESSURE_BUFFER"
	CodeCheckpointTruncated       = "E_CHECKPOINT_TRUNCATED"
	CodeCheckpointCorrupt         = "E_CHECKPOINT_CORRUPT"
	CodeCheckpointVersionMismatch = "E_CHECKPOINT_VERSION_MISMATCH"
	CodeCheckpointIncomplete      = "E_CHECKPOINT_INCOMPLETE"
	CodeCheckpointTooLarge        = "E_CHECKPOINT_TOO_LARGE"
	CodeEditInvalid               = "E_EDIT_INVALID"
)

// BizError is a stable, user-facing business error. Version and Location are
// always populated when the error originates from a project document.
type BizError struct {
	Code     string `json:"code"`
	Version  string `json:"version,omitempty"`
	Location string `json:"location,omitempty"`
	Message  string `json:"message"`
}

func (e *BizError) Error() string {
	if e.Version == "" && e.Location == "" {
		return fmt.Sprintf("%s: %s", e.Code, e.Message)
	}
	if e.Location == "" {
		return fmt.Sprintf("%s [%s]: %s", e.Code, e.Version, e.Message)
	}
	return fmt.Sprintf("%s [%s %s]: %s", e.Code, e.Version, e.Location, e.Message)
}

func bizError(code, version, location, format string, args ...any) *BizError {
	return &BizError{
		Code:     code,
		Version:  version,
		Location: location,
		Message:  fmt.Sprintf(format, args...),
	}
}
