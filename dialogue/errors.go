package dialogue

import (
	"errors"
	"fmt"
	"strings"
)

// Code is a stable machine-readable error code.
type Code string

const (
	CodeSchema  Code = "E_SCHEMA"
	CodeLossy   Code = "E_LOSSY"
	CodeOVF     Code = "E_OVF"
	CodeLoud    Code = "E_LOUD"
	CodeState   Code = "E_STATE"
	CodeDiff    Code = "E_DIFF"
	CodeIO      Code = "E_IO"
	CodeDefault Code = "I_DEFAULT"
)

// Error carries the diagnostic context required by the kernel contract.
type Error struct {
	Code       Code     `json:"code"`
	DialogueID string   `json:"dialogue_id,omitempty"`
	Path       string   `json:"path,omitempty"`
	Field      string   `json:"field,omitempty"`
	Message    string   `json:"message"`
	Operands   []int64  `json:"operands,omitempty"`
	SegmentID  string   `json:"segment_id,omitempty"`
	FrameIndex *int     `json:"frame_index,omitempty"`
	Values     []string `json:"values,omitempty"`

	TargetLoudness int64    `json:"target_loudness,omitempty"`
	MinReachable   int64    `json:"min_reachable,omitempty"`
	MaxReachable   int64    `json:"max_reachable,omitempty"`
	LimitedIDs     []string `json:"limited_segment_ids,omitempty"`
}

func (e *Error) Error() string {
	if e == nil {
		return ""
	}
	var b strings.Builder
	b.WriteString(string(e.Code))
	if e.DialogueID != "" {
		b.WriteString(" dialogue=")
		b.WriteString(e.DialogueID)
	}
	if e.Path != "" {
		b.WriteString(" path=")
		b.WriteString(e.Path)
	}
	if e.SegmentID != "" {
		b.WriteString(" segment=")
		b.WriteString(e.SegmentID)
	}
	if e.FrameIndex != nil {
		fmt.Fprintf(&b, " frame=%d", *e.FrameIndex)
	}
	if len(e.Operands) > 0 {
		fmt.Fprintf(&b, " operands=%v", e.Operands)
	}
	if e.Message != "" {
		b.WriteString(": ")
		b.WriteString(e.Message)
	}
	return b.String()
}

func newError(code Code, dialogueID, path, message string) *Error {
	return &Error{Code: code, DialogueID: dialogueID, Path: path, Message: message}
}

// CodeOf returns the stable code of err, if it is a kernel error.
func CodeOf(err error) Code {
	var ke *Error
	if errors.As(err, &ke) {
		return ke.Code
	}
	return ""
}
