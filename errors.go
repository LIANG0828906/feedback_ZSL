package lipsync

import (
	"errors"
	"fmt"
)

// Code is a stable, machine-readable domain error code.
type Code string

const (
	CodeSchema Code = "E_SCHEMA"
	CodeOrder  Code = "E_ORDER"
	CodeRate   Code = "E_RATE"
	CodeRange  Code = "E_RANGE"
	CodeSched  Code = "E_SCHED"
	CodeLip    Code = "E_LIP"
	CodeState  Code = "E_STATE"
	CodeStage  Code = "E_STAGE"
)

// DomainError carries enough context for a game integration to locate a
// malformed or unschedulable dialogue without scraping an error string.
type DomainError struct {
	Code       Code   `json:"code"`
	DialogueID string `json:"dialogue_id,omitempty"`
	Field      string `json:"field,omitempty"`
	Path       string `json:"path,omitempty"`
	Frame      *int   `json:"frame,omitempty"`
	Window     *int   `json:"window,omitempty"`
	Stage      string `json:"stage,omitempty"`
	Reason     string `json:"reason,omitempty"`
	Sample     *int64 `json:"sample,omitempty"`
	FPS        *int   `json:"fps,omitempty"`
	SampleRate *int   `json:"sample_rate,omitempty"`
	Left       string `json:"left,omitempty"`
	Right      string `json:"right,omitempty"`
	Current    string `json:"current,omitempty"`
	Expected   string `json:"expected,omitempty"`
	Message    string `json:"message"`
	Cause      string `json:"cause,omitempty"`
}

func (e *DomainError) Error() string {
	location := ""
	if e.DialogueID != "" {
		location += " dialogue=" + e.DialogueID
	}
	if e.Field != "" {
		location += " field=" + e.Field
	}
	if e.Frame != nil {
		location += fmt.Sprintf(" frame=%d", *e.Frame)
	}
	if e.Window != nil {
		location += fmt.Sprintf(" window=%d", *e.Window)
	}
	if e.Message == "" {
		return fmt.Sprintf("%s%s", e.Code, location)
	}
	return fmt.Sprintf("%s%s: %s", e.Code, location, e.Message)
}

func intPtr(v int) *int       { return &v }
func int64Ptr(v int64) *int64 { return &v }

func newError(code Code, dialogueID, field, message string) *DomainError {
	return &DomainError{
		Code:       code,
		DialogueID: dialogueID,
		Field:      field,
		Message:    message,
	}
}

func asDomainError(err error) (*DomainError, bool) {
	var de *DomainError
	if errors.As(err, &de) {
		return de, true
	}
	return nil, false
}

// ErrorCode returns the domain code of err, or an empty string.
func ErrorCode(err error) Code {
	if de, ok := asDomainError(err); ok {
		return de.Code
	}
	return ""
}
