package loudness

import (
	"errors"
	"fmt"
	"strings"
)

// Code is a stable domain error code returned by the component.
type Code string

const (
	ESchema Code = "E_SCHEMA"
	ELossy  Code = "E_LOSSY"
	EOvF    Code = "E_OVF"
	EPara   Code = "E_PARA"
	ELoud   Code = "E_LOUD"
	EState  Code = "E_STATE"
)

// Loss describes one value that cannot be represented by an export target.
type Loss struct {
	Path        string `json:"path"`
	Value       string `json:"value"`
	Reason      string `json:"reason"`
	Disposition string `json:"disposition"`
}

// DomainError carries machine-readable diagnostics. Paths use JSONPath-like
// notation rooted at "$".
type DomainError struct {
	Code          Code      `json:"code"`
	Path          string    `json:"path,omitempty"`
	Field         string    `json:"field,omitempty"`
	SourceVersion Version   `json:"sourceVersion,omitempty"`
	Operands      []int64   `json:"operands,omitempty"`
	LegalMin      int64     `json:"legalMin,omitempty"`
	LegalMax      int64     `json:"legalMax,omitempty"`
	Target        int64     `json:"target,omitempty"`
	Reachable     int64     `json:"reachableUpper,omitempty"`
	LimitedClips  []int     `json:"limitedClipIds,omitempty"`
	FromState     ClipState `json:"fromState,omitempty"`
	ToState       ClipState `json:"toState,omitempty"`
	CommitCount   int       `json:"committedClips,omitempty"`
	ClipTotal     int       `json:"totalClips,omitempty"`
	Losses        []Loss    `json:"losses,omitempty"`
	Detail        string    `json:"detail,omitempty"`
	Cause         error     `json:"-"`
}

func (e *DomainError) Error() string {
	var b strings.Builder
	b.WriteString(string(e.Code))
	if e.Path != "" {
		b.WriteString(" path=")
		b.WriteString(e.Path)
	}
	if e.Field != "" {
		b.WriteString(" field=")
		b.WriteString(e.Field)
	}
	if e.SourceVersion != "" {
		b.WriteString(" source=")
		b.WriteString(string(e.SourceVersion))
	}
	if len(e.Operands) > 0 {
		b.WriteString(" operands=[")
		for i, operand := range e.Operands {
			if i > 0 {
				b.WriteString(",")
			}
			fmt.Fprintf(&b, "%d", operand)
		}
		b.WriteString("]")
	}
	if e.Code == ELoud {
		fmt.Fprintf(&b, " target=%d reachableUpper=%d limitedClipIds=%v", e.Target, e.Reachable, e.LimitedClips)
	}
	if e.Code == EState {
		fmt.Fprintf(&b, " transition=%s->%s committed=%d/%d", e.FromState, e.ToState, e.CommitCount, e.ClipTotal)
	}
	if e.Detail != "" {
		b.WriteString(" detail=")
		b.WriteString(e.Detail)
	}
	return b.String()
}

func (e *DomainError) Unwrap() error { return e.Cause }

func newError(code Code, path, detail string, cause error) *DomainError {
	return &DomainError{Code: code, Path: path, Detail: detail, Cause: cause}
}

// HasCode reports whether err, or any wrapped error, is a DomainError with code.
func HasCode(err error, code Code) bool {
	var domainErr *DomainError
	return errors.As(err, &domainErr) && domainErr.Code == code
}

// AsDomainError extracts a DomainError if present.
func AsDomainError(err error) (*DomainError, bool) {
	var domainErr *DomainError
	ok := errors.As(err, &domainErr)
	return domainErr, ok
}
