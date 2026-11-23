// Package errs defines the stable error-code vocabulary used across the
// caption timing pipeline. Every failure that crosses a component boundary
// is carried as a *Error so callers can branch on the code and locate the
// offending caption / field.
package errs

import "fmt"

// Error codes returned by the pipeline.
const (
	ESchema = "E_SCHEMA" // manifest structure / reference violation
	EPara   = "E_PARA"   // illegal parameter value
	ESched  = "E_SCHED"  // character scheduling impossible
	EDecide = "E_DECIDE" // non-deterministic trade-off decision
	EBound  = "E_BOUND"  // measured deviation exceeds reported bound
	EState  = "E_STATE"  // illegal segment state transition
	EDiff   = "E_DIFF"   // processing paths disagree
	EIO     = "E_IO"     // persistence layer failure
)

// Error is a coded error with optional localisation fields.
type Error struct {
	Code      string `json:"code"`
	CaptionID int    `json:"captionId,omitempty"` // valid only when HasCap
	HasCap    bool   `json:"hasCaption,omitempty"`
	Field     string `json:"field,omitempty"`
	Term      string `json:"term,omitempty"`     // error-term name for E_BOUND
	Measured  int64  `json:"measured,omitempty"` // measured value for E_BOUND
	CharIdx   int    `json:"charIdx,omitempty"`
	HasChar   bool   `json:"hasChar,omitempty"`
	Msg       string `json:"msg"`
}

func (e *Error) Error() string {
	s := e.Code
	if e.HasCap {
		s += fmt.Sprintf(" caption=%d", e.CaptionID)
	}
	if e.HasChar {
		s += fmt.Sprintf(" char=%d", e.CharIdx)
	}
	if e.Field != "" {
		s += " field=" + e.Field
	}
	if e.Term != "" {
		s += fmt.Sprintf(" term=%s measured=%d", e.Term, e.Measured)
	}
	if e.Msg != "" {
		s += ": " + e.Msg
	}
	return s
}

// Code extracts the stable error code from err, or "" if not a coded error.
func Code(err error) string {
	if e, ok := err.(*Error); ok {
		return e.Code
	}
	return ""
}

func Schema(captionID int, field, msg string) *Error {
	return &Error{Code: ESchema, CaptionID: captionID, HasCap: captionID >= 0, Field: field, Msg: msg}
}

func Para(field, msg string) *Error {
	return &Error{Code: EPara, Field: field, Msg: msg}
}

func Sched(captionID, charIdx int, msg string) *Error {
	return &Error{Code: ESched, CaptionID: captionID, HasCap: true, CharIdx: charIdx, HasChar: charIdx >= 0, Msg: msg}
}

func Decide(msg string) *Error {
	return &Error{Code: EDecide, Msg: msg}
}

func Bound(captionID int, term string, measured int64, msg string) *Error {
	return &Error{Code: EBound, CaptionID: captionID, HasCap: true, Term: term, Measured: measured, Msg: msg}
}

func State(msg string) *Error {
	return &Error{Code: EState, Msg: msg}
}

func Diff(captionID, charIdx int, field, msg string) *Error {
	return &Error{Code: EDiff, CaptionID: captionID, HasCap: captionID >= 0, CharIdx: charIdx, HasChar: charIdx >= 0, Field: field, Msg: msg}
}

func IO(msg string) *Error {
	return &Error{Code: EIO, Msg: msg}
}
