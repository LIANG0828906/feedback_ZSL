package model

import (
	"errors"
	"fmt"
)

// Stable error codes used by the acceptance surface.
const (
	CodeDecode = "E_DECODE"
	CodeValue  = "E_VAL"
	CodeSeam   = "E_SEAM"
	CodeDiff   = "E_DIFF"
	CodeSnap   = "E_SNAP"
	CodeCancel = "E_CANCEL"
	CodeIO     = "E_IO"
)

// CodedError carries machine-readable context in addition to a message.
type CodedError struct {
	Code       string       `json:"code"`
	Message    string       `json:"message"`
	ChunkID    string       `json:"chunk_id,omitempty"`
	Field      string       `json:"field,omitempty"`
	Point      *VertexPoint `json:"point,omitempty"`
	World      *Vec2        `json:"world,omitempty"`
	Channel    string       `json:"channel,omitempty"`
	EdgeID     string       `json:"edge_id,omitempty"`
	Time       *int         `json:"time,omitempty"`
	Reason     string       `json:"reason,omitempty"`
	Dependency string       `json:"dependency,omitempty"`
	Cause      error        `json:"-"`
}

// Error implements error.
func (e *CodedError) Error() string {
	if e == nil {
		return "<nil>"
	}
	if e.Cause != nil {
		return fmt.Sprintf("%s: %s: %v", e.Code, e.Message, e.Cause)
	}
	return fmt.Sprintf("%s: %s", e.Code, e.Message)
}

// Unwrap exposes the underlying cause.
func (e *CodedError) Unwrap() error { return e.Cause }

// NewError creates a coded error.
func NewError(code, format string, args ...any) *CodedError {
	return &CodedError{Code: code, Message: fmt.Sprintf(format, args...)}
}

// WithChunk attaches a chunk identifier.
func (e *CodedError) WithChunk(chunkID string) *CodedError {
	e.ChunkID = chunkID
	return e
}

// WithField attaches a JSON-style field path.
func (e *CodedError) WithField(field string) *CodedError {
	e.Field = field
	return e
}

// WithPoint attaches a vertex coordinate.
func (e *CodedError) WithPoint(p VertexPoint) *CodedError {
	e.Point = &p
	return e
}

// WithWorld attaches a world coordinate.
func (e *CodedError) WithWorld(p Vec2) *CodedError {
	e.World = &p
	return e
}

// WithChannel attaches a radiance or visibility channel.
func (e *CodedError) WithChannel(channel string) *CodedError {
	e.Channel = channel
	return e
}

// WithEdge attaches a shared edge identifier.
func (e *CodedError) WithEdge(edgeID string) *CodedError {
	e.EdgeID = edgeID
	return e
}

// WithTime attaches a logical time.
func (e *CodedError) WithTime(t int) *CodedError {
	e.Time = &t
	return e
}

// CodeOf returns the stable code carried by err, if any.
func CodeOf(err error) string {
	var coded *CodedError
	if errors.As(err, &coded) {
		return coded.Code
	}
	return ""
}

// AsCoded returns the coded error carried by err.
func AsCoded(err error) (*CodedError, bool) {
	var coded *CodedError
	ok := errors.As(err, &coded)
	return coded, ok
}
