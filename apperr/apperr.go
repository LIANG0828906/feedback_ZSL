// Package apperr defines the stable, machine-readable error contract used by
// the loudness workbench.
package apperr

import (
	"encoding/json"
	"errors"
	"fmt"
)

const (
	CodeVersion    = "E_VER"
	CodeSchema     = "E_SCHEMA"
	CodeParameter  = "E_PARA"
	CodeClip       = "E_CLIP"
	CodeLossy      = "E_LOSSY"
	CodeState      = "E_STATE"
	CodeDecode     = "E_DECODE"
	CodeDivergence = "E_DIVERGE"
	CodeCheckpoint = "E_CHECKPOINT"
	CodeCancelled  = "E_CANCEL"
	CodeInternal   = "E_INTERNAL"
)

// Error is serializable so CLI and HTTP surfaces report the same diagnostics.
type Error struct {
	Code         string   `json:"code"`
	Message      string   `json:"message"`
	Field        string   `json:"field,omitempty"`
	Path         string   `json:"path,omitempty"`
	SegmentIndex *int     `json:"segment_index,omitempty"`
	SegmentID    string   `json:"segment_id,omitempty"`
	SampleIndex  *int64   `json:"sample_index,omitempty"`
	FrameIndex   *int64   `json:"frame_index,omitempty"`
	Channel      *int     `json:"channel,omitempty"`
	Actual       any      `json:"actual,omitempty"`
	Expected     any      `json:"expected,omitempty"`
	Limit        any      `json:"limit,omitempty"`
	Left         any      `json:"left,omitempty"`
	Right        any      `json:"right,omitempty"`
	Trace        []string `json:"degradation_trace,omitempty"`
	Cause        error    `json:"-"`
}

func New(code, message string) *Error {
	return &Error{Code: code, Message: message}
}

func Wrap(code, message string, cause error) *Error {
	return &Error{Code: code, Message: message, Cause: cause}
}

func (e *Error) Error() string {
	if e == nil {
		return ""
	}
	location := ""
	if e.Path != "" {
		location = " path=" + e.Path
	} else if e.Field != "" {
		location = " field=" + e.Field
	}
	if e.Cause != nil {
		return fmt.Sprintf("%s: %s%s: %v", e.Code, e.Message, location, e.Cause)
	}
	return fmt.Sprintf("%s: %s%s", e.Code, e.Message, location)
}

func (e *Error) Unwrap() error {
	return e.Cause
}

func (e *Error) WithField(field string) *Error {
	e.Field = field
	return e
}

func (e *Error) WithPath(path string) *Error {
	e.Path = path
	return e
}

func (e *Error) WithSegment(index int, id string) *Error {
	idx := index
	e.SegmentIndex = &idx
	e.SegmentID = id
	return e
}

func (e *Error) WithActual(v any) *Error {
	e.Actual = v
	return e
}

func (e *Error) WithExpected(v any) *Error {
	e.Expected = v
	return e
}

func (e *Error) WithLimit(v any) *Error {
	e.Limit = v
	return e
}

func (e *Error) WithSides(left, right any) *Error {
	e.Left = left
	e.Right = right
	return e
}

func (e *Error) WithTrace(trace []string) *Error {
	e.Trace = trace
	return e
}

func (e *Error) WithSample(index int64, frame int64, channel int) *Error {
	e.SampleIndex = &index
	e.FrameIndex = &frame
	e.Channel = &channel
	return e
}

// As extracts an *Error without losing wrapped errors.
func As(err error) (*Error, bool) {
	var target *Error
	if errors.As(err, &target) {
		return target, true
	}
	return nil, false
}

func Code(err error) string {
	if e, ok := As(err); ok {
		return e.Code
	}
	return ""
}

func IsCode(err error, code string) bool {
	return Code(err) == code
}

// JSON renders an error for CLI output.
func JSON(err error) string {
	if err == nil {
		return "null"
	}
	if e, ok := As(err); ok {
		data, marshalErr := json.MarshalIndent(e, "", "  ")
		if marshalErr == nil {
			return string(data)
		}
	}
	data, _ := json.MarshalIndent(map[string]string{"code": CodeInternal, "message": err.Error()}, "", "  ")
	return string(data)
}
