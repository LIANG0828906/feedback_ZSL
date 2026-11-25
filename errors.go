package skillcore

import (
	"errors"
	"fmt"
	"strings"
)

// Code is a stable machine-readable domain error code.
type Code string

const (
	CodeVersion  Code = "E_VERSION"
	CodeLossy    Code = "E_LOSSY"
	CodeOverflow Code = "E_OVER"
	CodeConflict Code = "E_CONFLICT"
	CodeState    Code = "E_STATE"
	CodeInput    Code = "E_INPUT"
	CodeWindow   Code = "E_WINDOW"
	CodeInternal Code = "E_INTERNAL"
)

// Loss describes a value or shape that cannot be represented by a target format.
type Loss struct {
	Code        Code   `json:"code"`
	Path        string `json:"path"`
	Value       string `json:"value,omitempty"`
	Disposition string `json:"disposition"`
	Reason      string `json:"reason"`
}

// FillTrace records an explicit default application during an upgrade.
type FillTrace struct {
	Path         string `json:"path"`
	OldValue     string `json:"oldValue"`
	DefaultValue string `json:"defaultValue"`
	Source       string `json:"source"`
}

// Error carries actionable context without relying on error text parsing.
type Error struct {
	Code         Code     `json:"code"`
	Path         string   `json:"path,omitempty"`
	Message      string   `json:"message"`
	Unit         string   `json:"unit,omitempty"`
	Field        string   `json:"field,omitempty"`
	Tick         int64    `json:"tick,omitempty"`
	HasTick      bool     `json:"hasTick,omitempty"`
	SkillID      string   `json:"skillId,omitempty"`
	ConflictType string   `json:"conflictType,omitempty"`
	Skills       []string `json:"skills,omitempty"`
	Losses       []Loss   `json:"losses,omitempty"`
	CurrentState string   `json:"currentState,omitempty"`
	NextState    string   `json:"nextState,omitempty"`
	Cause        error    `json:"-"`
}

func (e *Error) Error() string {
	if e == nil {
		return ""
	}
	var b strings.Builder
	b.WriteString(string(e.Code))
	if e.Path != "" {
		b.WriteString(" [")
		b.WriteString(e.Path)
		b.WriteByte(']')
	}
	if e.Message != "" {
		b.WriteString(": ")
		b.WriteString(e.Message)
	}
	if e.HasTick {
		fmt.Fprintf(&b, " (tick=%d", e.Tick)
		if e.SkillID != "" {
			b.WriteString(", skill=")
			b.WriteString(e.SkillID)
		}
		b.WriteByte(')')
	}
	if len(e.Skills) > 0 {
		b.WriteString(" (skills=")
		b.WriteString(strings.Join(e.Skills, ","))
		b.WriteByte(')')
	}
	if e.Unit != "" {
		b.WriteString(" (unit=")
		b.WriteString(e.Unit)
		b.WriteByte(')')
	}
	if e.Cause != nil {
		b.WriteString(": ")
		b.WriteString(e.Cause.Error())
	}
	return b.String()
}

func (e *Error) Unwrap() error { return e.Cause }

func IsCode(err error, code Code) bool {
	var domainErr *Error
	return errors.As(err, &domainErr) && domainErr.Code == code
}

func domainErr(code Code, path, message string) *Error {
	return &Error{Code: code, Path: path, Message: message}
}

func wrapErr(code Code, path, message string, cause error) *Error {
	return &Error{Code: code, Path: path, Message: message, Cause: cause}
}
