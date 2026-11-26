package model

import "fmt"

type ErrorCode string

const (
	CodeDecode     ErrorCode = "E_DECODE"
	CodeFit        ErrorCode = "E_FIT"
	CodeState      ErrorCode = "E_STATE"
	CodeCheckpoint ErrorCode = "E_CHECKPOINT"
	CodeBudget     ErrorCode = "E_BUDGET"
	CodeConflict   ErrorCode = "E_CONFLICT"
)

type Error struct {
	Code       ErrorCode      `json:"code"`
	DialogueID string         `json:"dialogue_id,omitempty"`
	Field      string         `json:"field,omitempty"`
	Message    string         `json:"message"`
	Details    map[string]any `json:"details,omitempty"`
}

func (e *Error) Error() string {
	if e == nil {
		return ""
	}
	if e.DialogueID != "" && e.Field != "" {
		return fmt.Sprintf("%s dialogue_id=%s field=%s: %s", e.Code, e.DialogueID, e.Field, e.Message)
	}
	if e.DialogueID != "" {
		return fmt.Sprintf("%s dialogue_id=%s: %s", e.Code, e.DialogueID, e.Message)
	}
	if e.Field != "" {
		return fmt.Sprintf("%s field=%s: %s", e.Code, e.Field, e.Message)
	}
	return fmt.Sprintf("%s: %s", e.Code, e.Message)
}

func DecodeError(dialogueID, field, format string, args ...any) *Error {
	return &Error{Code: CodeDecode, DialogueID: dialogueID, Field: field, Message: fmt.Sprintf(format, args...)}
}

func FitError(dialogueID string, phonemeIndex int, field, format string, args ...any) *Error {
	details := map[string]any{"phoneme_index": phonemeIndex}
	return &Error{Code: CodeFit, DialogueID: dialogueID, Field: field, Message: fmt.Sprintf(format, args...), Details: details}
}

func StateError(dialogueID, field, format string, args ...any) *Error {
	return &Error{Code: CodeState, DialogueID: dialogueID, Field: field, Message: fmt.Sprintf(format, args...)}
}

func CheckpointError(dialogueID, field, format string, args ...any) *Error {
	return &Error{Code: CodeCheckpoint, DialogueID: dialogueID, Field: field, Message: fmt.Sprintf(format, args...)}
}

func AsError(err error) (*Error, bool) {
	if err == nil {
		return nil, false
	}
	var domainErr *Error
	for err != nil {
		if e, ok := err.(*Error); ok {
			domainErr = e
			return domainErr, true
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
