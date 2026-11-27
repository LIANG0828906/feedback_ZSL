package stage

import (
	"errors"
	"fmt"
	"strings"
)

// Code is the stable machine-readable error code used by the domain component.
type Code string

const (
	CodeSchema   Code = "E_SCHEMA"
	CodeLossy    Code = "E_LOSSY"
	CodeOverflow Code = "E_OVF"
	CodeParam    Code = "E_PARA"
	CodeValue    Code = "E_VAL"
	CodeState    Code = "E_STATE"
	CodeVersion  Code = "E_VER"
	CodeDiverge  Code = "E_DIVERGE"
)

// Error carries the context required by the diagnostics and HTTP layers.
// JSON tags intentionally make errors safe to return from diagnostics endpoints.
type Error struct {
	Code         Code     `json:"code"`
	Message      string   `json:"message"`
	Version      string   `json:"version,omitempty"`
	Field        string   `json:"field,omitempty"`
	FixtureID    string   `json:"fixture_id,omitempty"`
	CueID        string   `json:"cue_id,omitempty"`
	WindowNumber int      `json:"window_number,omitempty"`
	FieldName    string   `json:"field_name,omitempty"`
	Operands     []int64  `json:"operands,omitempty"`
	Coordinate   []int64  `json:"coordinate_mm,omitempty"`
	Actual       any      `json:"actual,omitempty"`
	Expected     any      `json:"expected,omitempty"`
	Rounding     string   `json:"rounding,omitempty"`
	Details      []string `json:"details,omitempty"`
}

func (e *Error) Error() string {
	if e == nil {
		return "<nil>"
	}
	var b strings.Builder
	b.WriteString(string(e.Code))
	if e.Message != "" {
		b.WriteString(": ")
		b.WriteString(e.Message)
	}
	if e.Version != "" {
		fmt.Fprintf(&b, " [version=%s]", e.Version)
	}
	if e.Field != "" {
		fmt.Fprintf(&b, " [field=%s]", e.Field)
	}
	if e.FixtureID != "" {
		fmt.Fprintf(&b, " [fixture=%s]", e.FixtureID)
	}
	if e.WindowNumber != 0 {
		fmt.Fprintf(&b, " [window=%d]", e.WindowNumber)
	}
	if len(e.Operands) >= 2 {
		fmt.Fprintf(&b, " [operands=%d,%d]", e.Operands[0], e.Operands[1])
	}
	return b.String()
}

// Is lets errors.Is(err, ErrSchema) work while retaining the detailed Error.
func (e *Error) Is(target error) bool {
	if e == nil || target == nil {
		return false
	}
	var t *Error
	if !errors.As(target, &t) {
		return false
	}
	return e.Code == t.Code
}

var (
	ErrSchema   = &Error{Code: CodeSchema}
	ErrLossy    = &Error{Code: CodeLossy}
	ErrOverflow = &Error{Code: CodeOverflow}
	ErrParam    = &Error{Code: CodeParam}
	ErrValue    = &Error{Code: CodeValue}
	ErrState    = &Error{Code: CodeState}
	ErrVersion  = &Error{Code: CodeVersion}
	ErrDiverge  = &Error{Code: CodeDiverge}
)

func domainError(code Code, message, field string) *Error {
	return &Error{Code: code, Message: message, Field: field, FieldName: field}
}

func AsError(err error) *Error {
	var target *Error
	if errors.As(err, &target) {
		return target
	}
	return nil
}

func HasCode(err error, code Code) bool {
	var target *Error
	return errors.As(err, &target) && target.Code == code
}

// Loss is one explicitly reported downgrade decision. A loss never hides the
// exported artifact: callers receive both the bytes and the E_LOSSY list.
type Loss struct {
	Code          Code   `json:"code"`
	Field         string `json:"field"`
	FixtureID     string `json:"fixture_id,omitempty"`
	CueID         string `json:"cue_id,omitempty"`
	SourceVersion string `json:"source_version"`
	TargetVersion string `json:"target_version"`
	Reason        string `json:"reason"`
	Disposition   string `json:"disposition"`
	Original      string `json:"original,omitempty"`
	Exported      string `json:"exported,omitempty"`
}

func (l Loss) Error() string {
	return fmt.Sprintf("%s: %s (%s -> %s): %s; disposition=%s", l.Code, l.Field, l.SourceVersion, l.TargetVersion, l.Reason, l.Disposition)
}
