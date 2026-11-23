package morph

import (
	"errors"
	"fmt"
)

// Code is a stable machine-readable error code used by the kernel and CLI.
type Code string

const (
	CodeDecode  Code = "E_DECODE"
	CodePath    Code = "E_PATH"
	CodeKey     Code = "E_KEY"
	CodeStyle   Code = "E_STYLE"
	CodeParam   Code = "E_PARA"
	CodeScale   Code = "E_SCALE"
	CodeState   Code = "E_STATE"
	CodeDiverge Code = "E_DIVERGE"
)

// Error carries the diagnostic context required by the error-code contract.
type Error struct {
	Code     Code    `json:"code"`
	PathID   string  `json:"path_id,omitempty"`
	Field    string  `json:"field,omitempty"`
	Sample   int     `json:"sample"`
	Time     float64 `json:"time,omitempty"`
	Metric   string  `json:"metric,omitempty"`
	Measured string  `json:"measured,omitempty"`
	Expected string  `json:"expected,omitempty"`
	PixelX   int     `json:"-"`
	PixelY   int     `json:"-"`
	Pixel    []int   `json:"pixel,omitempty"`
	HasPixel bool    `json:"-"`
	Message  string  `json:"message"`
	Cause    error   `json:"-"`
}

func (e *Error) Error() string {
	if e == nil {
		return ""
	}
	ctx := ""
	if e.PathID != "" {
		ctx += " path=" + e.PathID
	}
	if e.Field != "" {
		ctx += " field=" + e.Field
	}
	if e.Sample >= 0 {
		ctx += fmt.Sprintf(" sample=%d", e.Sample)
	}
	if e.Sample >= 0 {
		ctx += fmt.Sprintf(" time=%.9g", e.Time)
	}
	if e.HasPixel {
		ctx += fmt.Sprintf(" pixel=(%d,%d)", e.PixelX, e.PixelY)
	}
	if e.Metric != "" {
		ctx += " metric=" + e.Metric
	}
	if e.Measured != "" || e.Expected != "" {
		ctx += fmt.Sprintf(" measured=%s expected=%s", e.Measured, e.Expected)
	}
	if e.Cause != nil {
		return fmt.Sprintf("%s:%s %s: %v", e.Code, ctx, e.Message, e.Cause)
	}
	return fmt.Sprintf("%s:%s %s", e.Code, ctx, e.Message)
}

func (e *Error) Unwrap() error { return e.Cause }

func newError(code Code, message string) *Error {
	return &Error{Code: code, Sample: -1, Message: message}
}

func withPath(e *Error, id string) *Error {
	e.PathID = id
	return e
}

func withField(e *Error, field string) *Error {
	e.Field = field
	return e
}

func withSample(e *Error, sample int, t float64) *Error {
	e.Sample = sample
	e.Time = t
	return e
}

// CodeOf extracts a stable error code. Plain errors are mapped to E_DECODE
// only when no more specific classification exists.
func CodeOf(err error) Code {
	var e *Error
	if errors.As(err, &e) {
		return e.Code
	}
	return CodeDecode
}

// AsError exposes detailed error context when available.
func AsError(err error) (*Error, bool) {
	var e *Error
	ok := errors.As(err, &e)
	return e, ok
}
