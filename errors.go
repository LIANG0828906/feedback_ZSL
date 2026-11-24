package balance

import (
	"errors"
	"fmt"
)

const (
	CodeSchema     = "E_SCHEMA"
	CodeCycle      = "E_CYCLE"
	CodeRange      = "E_RANGE"
	CodeBudget     = "E_BUDGET"
	CodeState      = "E_STATE"
	CodeDiff       = "E_DIFF"
	CodeUnit       = "E_UNIT"
	CodeCooldown   = "E_COOLDOWN"
	CodeResource   = "E_RES"
	CodeInternal   = "E_INTERNAL"
	CodeCheckpoint = "E_CHECKPOINT"
	CodeIO         = "E_IO"
	CodeVerify     = "E_VERIFY"
)

// AppError is the stable error envelope used by the CLI and tests.
type AppError struct {
	Code         string `json:"code"`
	Object       string `json:"object,omitempty"`
	PlanID       string `json:"planId,omitempty"`
	ScenarioID   string `json:"scenarioId,omitempty"`
	MatchIndex   int    `json:"matchIndex,omitempty"`
	Turn         int    `json:"turn,omitempty"`
	Field        string `json:"field,omitempty"`
	Expected     string `json:"expected,omitempty"`
	Actual       string `json:"actual,omitempty"`
	ConflictRule string `json:"conflictRule,omitempty"`
	Detail       string `json:"detail,omitempty"`
}

func (e *AppError) Error() string {
	base := e.Code
	if e.Object != "" {
		base += " object=" + e.Object
	}
	if e.PlanID != "" {
		base += " plan=" + e.PlanID
	}
	if e.ScenarioID != "" {
		base += " scenario=" + e.ScenarioID
	}
	if e.MatchIndex > 0 {
		base += fmt.Sprintf(" match=%d", e.MatchIndex)
	}
	if e.Turn > 0 {
		base += fmt.Sprintf(" turn=%d", e.Turn)
	}
	if e.Field != "" {
		base += " field=" + e.Field
	}
	if e.ConflictRule != "" {
		base += " conflictRule=" + e.ConflictRule
	}
	if e.Detail != "" {
		base += ": " + e.Detail
	}
	return base
}

func newError(code, object, format string, args ...any) *AppError {
	return &AppError{Code: code, Object: object, Detail: fmt.Sprintf(format, args...)}
}

func ErrorCode(err error) string {
	var appErr *AppError
	if errors.As(err, &appErr) {
		return appErr.Code
	}
	return ""
}

func AsAppError(err error) *AppError {
	var appErr *AppError
	if errors.As(err, &appErr) {
		return appErr
	}
	return nil
}
