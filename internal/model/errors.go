package model

import "fmt"

type ErrorCode string

const (
	EOk                ErrorCode = "E_OK"
	EFieldRequired     ErrorCode = "E_FIELD_REQUIRED"
	EFieldAmbiguous    ErrorCode = "E_FIELD_AMBIGUOUS"
	ESchemaUnsupported ErrorCode = "E_SCHEMA_UNSUPPORTED"
	ESchemaVersion     ErrorCode = "E_SCHEMA_VERSION"
	EDupSegment        ErrorCode = "E_DUP_SEGMENT"
	EDupRef            ErrorCode = "E_DUP_SEGMENT_REF"
	EDupRecord         ErrorCode = "E_DUP_RECORD"
	EOrder             ErrorCode = "E_SEQUENCE_ORDER"
	EBaseConflict      ErrorCode = "E_SEQ_BASE_CONFLICT"
	EDepCycle          ErrorCode = "E_DEPENDENCY_CYCLE"
	EDepMissing        ErrorCode = "E_DEPENDENCY_MISSING"
	ESegmentTruncated  ErrorCode = "E_SEGMENT_TRUNCATED"
	ERecordIncomplete  ErrorCode = "E_RECORD_INCOMPLETE"
	ECPCorrupt         ErrorCode = "E_CHECKPOINT_TRUNCATED"
	EVersionMismatch   ErrorCode = "E_VERSION_MISMATCH"
	EBudgetThreads     ErrorCode = "E_BUDGET_THREADS"
	EBudgetMemory      ErrorCode = "E_BUDGET_MEMORY"
	EBackpressure      ErrorCode = "E_BACKPRESSURE"
	EStateConflict     ErrorCode = "E_STATE_CONFLICT"
	ECheckpointWrite   ErrorCode = "E_CHECKPOINT_WRITE"
	ESignalTerm        ErrorCode = "E_SIGNAL_TERM"
)

type Error struct {
	Code    ErrorCode      `json:"code"`
	Message string         `json:"message"`
	Locator string         `json:"locator,omitempty"`
	Details map[string]any `json:"details,omitempty"`
}

func (e *Error) Error() string {
	if e == nil {
		return string(EOk)
	}
	if e.Locator != "" {
		return fmt.Sprintf("%s at %s: %s", e.Code, e.Locator, e.Message)
	}
	return fmt.Sprintf("%s: %s", e.Code, e.Message)
}

func (e *Error) WithDetails(details map[string]any) *Error {
	if e == nil {
		return nil
	}
	if e.Details == nil {
		e.Details = map[string]any{}
	}
	for k, v := range details {
		e.Details[k] = v
	}
	return e
}

func NewError(code ErrorCode, locator, format string, args ...any) *Error {
	return &Error{Code: code, Locator: locator, Message: fmt.Sprintf(format, args...)}
}

func WrapError(code ErrorCode, locator string, err error) *Error {
	if err == nil {
		return nil
	}
	if be, ok := err.(*Error); ok {
		if be.Locator == "" {
			be.Locator = locator
		}
		return be
	}
	return &Error{Code: code, Locator: locator, Message: err.Error()}
}

func AsError(err error) *Error {
	if err == nil {
		return nil
	}
	if be, ok := err.(*Error); ok {
		return be
	}
	return &Error{Code: EStateConflict, Message: err.Error()}
}
