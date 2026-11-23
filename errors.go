package orchestrator

import "fmt"

// Error codes returned by the orchestration pipeline.
const (
	ERef   = "E_REF"   // missing field or dangling reference
	ERhy   = "E_RHY"   // rhythm weight mismatch / zero sum / overflow
	EPara  = "E_PARA"  // illegal policy or parameter value
	ECut   = "E_CUT"   // cutoff deadline cannot be satisfied
	EQueue = "E_QUEUE" // queue ordering conflict / missing resume position
	EState = "E_STATE" // illegal state transition
	EDiff  = "E_DIFF"  // cross-path result mismatch
)

// Error is the structured orchestration error. Expected/Actual carry
// code-specific payloads (e.g. for E_CUT: Expected=required cutoff
// duration, Actual=allowed limit; for E_RHY count mismatch:
// Expected/Actual weight counts).
type Error struct {
	Code       string `json:"code"`
	DialogueID string `json:"dialogue_id,omitempty"`
	Field      string `json:"field,omitempty"`
	Message    string `json:"message"`
	Expected   int64  `json:"expected,omitempty"`
	Actual     int64  `json:"actual,omitempty"`
	Position   int    `json:"position,omitempty"`
}

func (e *Error) Error() string {
	s := fmt.Sprintf("%s: %s", e.Code, e.Message)
	if e.DialogueID != "" {
		s += fmt.Sprintf(" (dialogue=%s)", e.DialogueID)
	}
	if e.Field != "" {
		s += fmt.Sprintf(" (field=%s)", e.Field)
	}
	return s
}

func newError(code, dialogueID, field, msg string) *Error {
	return &Error{Code: code, DialogueID: dialogueID, Field: field, Message: msg}
}
