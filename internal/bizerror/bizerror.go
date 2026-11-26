package bizerror

import (
	"errors"
	"fmt"
)

const (
	ESchemaUnsupported      = "E_SCHEMA_UNSUPPORTED"
	ESchemaConflict         = "E_SCHEMA_CONFLICT"
	EFieldRequired          = "E_FIELD_REQUIRED"
	EFieldType              = "E_FIELD_TYPE"
	EResolutionNegative     = "E_RESOLUTION_NEGATIVE"
	EResolutionZero         = "E_RESOLUTION_ZERO"
	EResolutionAreaInvalid  = "E_RESOLUTION_AREA_INVALID"
	EResolutionConflict     = "E_RESOLUTION_CONFLICT"
	EScaleZero              = "E_SCALE_ZERO"
	EScaleOutOfRange        = "E_SCALE_OUT_OF_RANGE"
	EScaleTickUndeclared    = "E_SCALE_TICK_UNDECLARED"
	EChromaEnumInvalid      = "E_CHROMA_ENUM_INVALID"
	EChromaBitsInvalid      = "E_CHROMA_BITS_INVALID"
	EThreadOutOfRange       = "E_THREAD_OUT_OF_RANGE"
	EBufferBudgetOutOfRange = "E_BUFFER_BUDGET_OUT_OF_RANGE"
	EBudgetOutOfRange       = "E_BUDGET_OUT_OF_RANGE"
	EOverflow               = "E_OVERFLOW"
	EResolutionTooSmall     = "E_RESOLUTION_TOO_SMALL"
	EScreenNotFound         = "E_SCREEN_NOT_FOUND"
	EScreenDuplicate        = "E_SCREEN_DUPLICATE"
	EScreenAlreadyActive    = "E_SCREEN_ALREADY_ACTIVE"
	EScreenNotActive        = "E_SCREEN_NOT_ACTIVE"
	EAdmissionRejected      = "E_ADMISSION_REJECTED"
	ETopologyEventInvalid   = "E_TOPOLOGY_EVENT_INVALID"
	ECheckpointTruncated    = "E_CHECKPOINT_TRUNCATED"
	ECheckpointChecksum     = "E_CHECKPOINT_CHECKSUM_MISMATCH"
	ECheckpointVersion      = "E_CHECKPOINT_VERSION_MISMATCH"
	ECheckpointNotFound     = "E_CHECKPOINT_NOT_FOUND"
	EInvalidEdit            = "E_INVALID_EDIT"
	EInternal               = "E_INTERNAL"
)

type Error struct {
	Code       string         `json:"code"`
	Message    string         `json:"message"`
	Path       string         `json:"path,omitempty"`
	ScreenID   string         `json:"screen_id,omitempty"`
	Tick       int64          `json:"tick,omitempty"`
	Constraint string         `json:"constraint,omitempty"`
	Details    map[string]any `json:"details,omitempty"`
}

func New(code, message string) *Error {
	return &Error{Code: code, Message: message}
}

func Newf(code, format string, args ...any) *Error {
	return &Error{Code: code, Message: fmt.Sprintf(format, args...)}
}

func (e *Error) Error() string {
	if e == nil {
		return ""
	}
	if e.Path != "" {
		return fmt.Sprintf("%s at %s: %s", e.Code, e.Path, e.Message)
	}
	return fmt.Sprintf("%s: %s", e.Code, e.Message)
}

func (e *Error) WithPath(path string) *Error {
	clone := *e
	clone.Path = path
	return &clone
}

func (e *Error) WithScreen(id string) *Error {
	clone := *e
	clone.ScreenID = id
	return &clone
}

func (e *Error) WithTick(tick int64) *Error {
	clone := *e
	clone.Tick = tick
	return &clone
}

func (e *Error) WithConstraint(id string) *Error {
	clone := *e
	clone.Constraint = id
	return &clone
}

func (e *Error) WithDetail(key string, value any) *Error {
	clone := *e
	if clone.Details == nil {
		clone.Details = map[string]any{}
	} else {
		details := make(map[string]any, len(clone.Details)+1)
		for k, v := range clone.Details {
			details[k] = v
		}
		clone.Details = details
	}
	clone.Details[key] = value
	return &clone
}

func As(err error) (*Error, bool) {
	var be *Error
	if errors.As(err, &be) {
		return be, true
	}
	return nil, false
}

func CodeOf(err error) string {
	if be, ok := As(err); ok {
		return be.Code
	}
	return ""
}

func IsCode(err error, code string) bool {
	return CodeOf(err) == code
}
