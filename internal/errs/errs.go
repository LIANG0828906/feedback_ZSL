// Package errs defines the stable error surface used by the glyph brush
// pipeline. Every domain error carries enough context to point at a glyph,
// contour, stroke segment and parameter.
package errs

import (
	"errors"
	"fmt"
)

// Code is a stable machine-readable error code.
type Code string

const (
	CodeSchema Code = "E_SCHEMA"
	CodeParam  Code = "E_PARA"
	CodeGrid   Code = "E_GRID"
	CodeSelf   Code = "E_SELF"
	CodeZero   Code = "E_ZLEN"
	CodeDens   Code = "E_DENS"
	CodeStage  Code = "E_STAGE"
	CodeDiff   Code = "E_DIFF"
)

// Error is a structured pipeline error. Optional coordinates and identifiers
// are serialized as zero values when not applicable.
type Error struct {
	Code         Code   `json:"code"`
	GlyphID      string `json:"glyphId,omitempty"`
	ContourIndex *int   `json:"contourIndex,omitempty"`
	SegmentID    int    `json:"segmentId,omitempty"`
	Param        string `json:"param,omitempty"`
	PixelX       *int   `json:"pixelX,omitempty"`
	PixelY       *int   `json:"pixelY,omitempty"`
	Message      string `json:"message"`
	Cause        error  `json:"-"`
}

func (e *Error) Error() string {
	if e == nil {
		return "<nil>"
	}
	msg := fmt.Sprintf("%s: %s", e.Code, e.Message)
	if e.GlyphID != "" {
		msg += fmt.Sprintf(" glyph=%s", e.GlyphID)
	}
	if e.ContourIndex != nil {
		msg += fmt.Sprintf(" contour=%d", *e.ContourIndex)
	}
	if e.SegmentID != 0 {
		msg += fmt.Sprintf(" segment=%d", e.SegmentID)
	}
	if e.Param != "" {
		msg += fmt.Sprintf(" param=%s", e.Param)
	}
	if e.PixelX != nil && e.PixelY != nil {
		msg += fmt.Sprintf(" pixel=(%d,%d)", *e.PixelX, *e.PixelY)
	}
	return msg
}

func (e *Error) Unwrap() error { return e.Cause }

// WithContour returns a shallow copy with a contour index attached.
func (e *Error) WithContour(index int) *Error {
	cp := *e
	cp.ContourIndex = &index
	return &cp
}

// WithSegment returns a shallow copy with a stroke segment attached.
func (e *Error) WithSegment(id int) *Error {
	cp := *e
	cp.SegmentID = id
	return &cp
}

// WithParam returns a shallow copy with a parameter name attached.
func (e *Error) WithParam(name string) *Error {
	cp := *e
	cp.Param = name
	return &cp
}

// WithGlyph returns a shallow copy with a glyph identifier attached.
func (e *Error) WithGlyph(id string) *Error {
	cp := *e
	cp.GlyphID = id
	return &cp
}

// WithPixel returns a shallow copy with a pixel coordinate attached.
func (e *Error) WithPixel(x, y int) *Error {
	cp := *e
	cp.PixelX = &x
	cp.PixelY = &y
	return &cp
}

// New creates a structured error.
func New(code Code, format string, args ...any) *Error {
	return &Error{Code: code, Message: fmt.Sprintf(format, args...)}
}

// Wrap creates a structured error around cause.
func Wrap(code Code, cause error, format string, args ...any) *Error {
	return &Error{Code: code, Message: fmt.Sprintf(format, args...), Cause: cause}
}

// As returns the first structured error in err's chain.
func As(err error) (*Error, bool) {
	var target *Error
	if errors.As(err, &target) {
		return target, true
	}
	return nil, false
}

// IsCode reports whether err's chain contains code.
func IsCode(err error, code Code) bool {
	if err == nil {
		return false
	}
	if batch, ok := err.(*BatchError); ok {
		for _, item := range batch.Errors {
			if IsCode(item, code) {
				return true
			}
		}
	}
	for err != nil {
		if e, ok := err.(*Error); ok && e.Code == code {
			return true
		}
		err = errors.Unwrap(err)
	}
	return false
}

// BatchError preserves every segment-level failure while exposing the first
// failure as the representative error.
type BatchError struct {
	Errors []*Error `json:"errors"`
}

func (b *BatchError) Error() string {
	if len(b.Errors) == 0 {
		return "batch: no errors"
	}
	return b.Errors[0].Error()
}

func (b *BatchError) Unwrap() error {
	if len(b.Errors) == 0 {
		return nil
	}
	return b.Errors[0]
}
