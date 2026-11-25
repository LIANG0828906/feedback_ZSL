package apperr

import (
	"errors"
	"fmt"
	"strings"
)

// Code is a stable machine-readable workbench error code.
type Code string

const (
	EVER     Code = "E_VER"
	ESCHEMA  Code = "E_SCHEMA"
	ELOSSY   Code = "E_LOSSY"
	EREG     Code = "E_REG"
	ESTATE   Code = "E_STATE"
	EHASH    Code = "E_HASH"
	EVERSION Code = "E_VERSION"
	EIO      Code = "E_IO"
	EPARAM   Code = "E_PARAM"
)

// Loss describes one field that cannot be represented by a target schema.
type Loss struct {
	Path        string `json:"path"`
	Value       any    `json:"value"`
	Target      int    `json:"targetVersion"`
	Disposition string `json:"disposition"`
	Semantic    bool   `json:"semanticLoss"`
}

// Error carries the stable fields asserted by the workbench acceptance tests.
type Error struct {
	Code          Code    `json:"code"`
	Message       string  `json:"message"`
	Path          string  `json:"path,omitempty"`
	SourceVersion int     `json:"sourceVersion,omitempty"`
	BandPair      string  `json:"bandPair,omitempty"`
	OffsetX       float64 `json:"offsetX,omitempty"`
	OffsetY       float64 `json:"offsetY,omitempty"`
	Measured      float64 `json:"measuredOffset,omitempty"`
	Limit         float64 `json:"limit,omitempty"`
	Segment       int     `json:"segment,omitempty"`
	FromState     string  `json:"fromState,omitempty"`
	ToState       string  `json:"toState,omitempty"`
	Losses        []Loss  `json:"losses,omitempty"`
}

func (e *Error) Error() string {
	var b strings.Builder
	b.WriteString(string(e.Code))
	if e.Path != "" {
		fmt.Fprintf(&b, " [%s]", e.Path)
	}
	if e.SourceVersion != 0 {
		fmt.Fprintf(&b, " schema v%d", e.SourceVersion)
	}
	if e.Segment != 0 {
		fmt.Fprintf(&b, " segment %d", e.Segment)
	}
	if e.Message != "" {
		fmt.Fprintf(&b, ": %s", e.Message)
	}
	if e.BandPair != "" {
		fmt.Fprintf(&b, " (%s, offset %.3f, limit %.3f)", e.BandPair, e.Measured, e.Limit)
	}
	if len(e.Losses) > 0 {
		fmt.Fprintf(&b, " (%d lossy fields)", len(e.Losses))
	}
	return b.String()
}

func New(code Code, format string, args ...any) *Error {
	return &Error{Code: code, Message: fmt.Sprintf(format, args...)}
}

func Schema(sourceVersion int, path, format string, args ...any) *Error {
	return &Error{
		Code:          ESCHEMA,
		Message:       fmt.Sprintf(format, args...),
		Path:          path,
		SourceVersion: sourceVersion,
	}
}

func Version(version int) *Error {
	return &Error{Code: EVER, SourceVersion: version, Message: fmt.Sprintf("unsupported schema version %d", version)}
}

func CodeOf(err error) Code {
	var ae *Error
	if errors.As(err, &ae) {
		return ae.Code
	}
	return ""
}

func IsCode(err error, code Code) bool {
	return CodeOf(err) == code
}
