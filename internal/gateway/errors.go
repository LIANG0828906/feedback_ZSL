package gateway

import "fmt"

const (
	CodeSchemaUnsupported     = "E_SCHEMA_UNSUPPORTED"
	CodeFieldRequired         = "E_FIELD_REQUIRED"
	CodeFieldAmbiguous        = "E_FIELD_AMBIGUOUS"
	CodeFieldType             = "E_FIELD_TYPE_INVALID"
	CodeNegativeBytes         = "E_NEGATIVE_BYTES"
	CodeWindowWidthInvalid    = "E_WINDOW_WIDTH_INVALID"
	CodeWindowSlideInvalid    = "E_WINDOW_SLIDE_INVALID"
	CodeBucketBoundaryInvalid = "E_BUCKET_BOUNDARY_INVALID"
	CodeBucketScaleOutOfRange = "E_BUCKET_SCALE_OUT_OF_RANGE"
	CodeDurationOverflow      = "E_DURATION_OVERFLOW"
	CodeDurationNegative      = "E_DURATION_NEGATIVE"
	CodeWatermarkBeforeCommit = "E_WATERMARK_BEFORE_COMMIT"
	CodeBudgetWorkersInvalid  = "E_BUDGET_WORKERS_INVALID"
	CodeBudgetBufferInvalid   = "E_BUDGET_BUFFER_INVALID"
	CodeBackpressure          = "E_BACKPRESSURE"
	CodeExportConstraint      = "E_EXPORT_BATCH_CONSTRAINT"
	CodeWindowOversized       = "E_EXPORT_WINDOW_OVERSIZED"
	CodeCheckpointOpen        = "E_CHECKPOINT_OPEN"
	CodeCheckpointCorrupt     = "E_CHECKPOINT_CORRUPT"
	CodeCheckpointVersion     = "E_CHECKPOINT_VERSION_MISMATCH"
	CodeCheckpointWrite       = "E_CHECKPOINT_WRITE"
	CodeCheckpointNotFound    = "E_CHECKPOINT_NOT_FOUND"
	CodeSignalGraceful        = "E_SIGNAL_GRACEFUL"
	CodeSourceDataReadOnly    = "E_SOURCE_DATA_READ_ONLY"
)

type Error struct {
	Code    string         `json:"code"`
	Message string         `json:"message"`
	Path    string         `json:"path,omitempty"`
	Context map[string]any `json:"context,omitempty"`
}

func (e *Error) Error() string {
	if e.Path != "" {
		return fmt.Sprintf("%s at %s: %s", e.Code, e.Path, e.Message)
	}
	return fmt.Sprintf("%s: %s", e.Code, e.Message)
}

func (e *Error) WithContext(key string, value any) *Error {
	if e.Context == nil {
		e.Context = map[string]any{}
	}
	e.Context[key] = value
	return e
}

func NewError(code, path, format string, args ...any) *Error {
	return &Error{Code: code, Path: path, Message: fmt.Sprintf(format, args...)}
}

func AsError(err error) (*Error, bool) {
	if err == nil {
		return nil, false
	}
	var ge *Error
	if ok := errorAs(err, &ge); ok {
		return ge, true
	}
	return nil, false
}

func errorAs(err error, target **Error) bool {
	for err != nil {
		if e, ok := err.(*Error); ok {
			*target = e
			return true
		}
		type unwrapper interface{ Unwrap() error }
		u, ok := err.(unwrapper)
		if !ok {
			return false
		}
		err = u.Unwrap()
	}
	return false
}
