package domain

import (
	"errors"
	"fmt"
)

const (
	CodeSchema    = "E_SCHEMA"
	CodePosition  = "E_POS"
	CodePriority  = "E_PRIO"
	CodeCycle     = "E_CYCLE"
	CodeDuplicate = "E_DUP"
	CodeConflict  = "E_CONFLICT"
	CodeCommit    = "E_COMMIT"
	CodeDiff      = "E_DIFF"
	CodePrecision = "E_PREC"
)

// AppError is the stable machine-readable error envelope used by the CLI and UI.
// Fields are optional but always preserved when known.
type AppError struct {
	Code      string   `json:"code"`
	Message   string   `json:"message"`
	SegmentID string   `json:"segment_id,omitempty"`
	ChannelID string   `json:"channel_id,omitempty"`
	LayerID   string   `json:"layer_id,omitempty"`
	Beat      *float64 `json:"beat,omitempty"`
	Tick      *int     `json:"tick,omitempty"`
	Channels  []string `json:"channels,omitempty"`
	Field     string   `json:"field,omitempty"`
	Left      any      `json:"left,omitempty"`
	Right     any      `json:"right,omitempty"`
	Cause     string   `json:"cause,omitempty"`
}

func (e *AppError) Error() string {
	if e == nil {
		return "<nil>"
	}
	msg := e.Code + ": " + e.Message
	if e.SegmentID != "" {
		msg += " segment=" + e.SegmentID
	}
	if e.ChannelID != "" {
		msg += " channel=" + e.ChannelID
	}
	if len(e.Channels) > 0 {
		msg += fmt.Sprintf(" channels=%v", e.Channels)
	}
	if e.LayerID != "" {
		msg += " layer=" + e.LayerID
	}
	if e.Beat != nil {
		msg += fmt.Sprintf(" beat=%g", *e.Beat)
	}
	if e.Tick != nil {
		msg += fmt.Sprintf(" tick=%d", *e.Tick)
	}
	return msg
}

func NewError(code, format string, args ...any) *AppError {
	return &AppError{Code: code, Message: fmt.Sprintf(format, args...)}
}

func (e *AppError) WithSegment(id string) *AppError {
	e.SegmentID = id
	return e
}

func (e *AppError) WithChannel(id string) *AppError {
	e.ChannelID = id
	return e
}

func (e *AppError) WithLayer(id string) *AppError {
	e.LayerID = id
	return e
}

func (e *AppError) WithBeat(beat float64) *AppError {
	e.Beat = &beat
	return e
}

func (e *AppError) WithTick(tick int) *AppError {
	e.Tick = &tick
	return e
}

func AsAppError(err error) (*AppError, bool) {
	var appErr *AppError
	if errors.As(err, &appErr) {
		return appErr, true
	}
	return nil, false
}

func CodeOf(err error) string {
	if appErr, ok := AsAppError(err); ok {
		return appErr.Code
	}
	return ""
}
