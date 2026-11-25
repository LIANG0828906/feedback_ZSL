package model

import (
	"errors"
	"fmt"
	"strings"
)

type Code string

const (
	CodeSchema   Code = "E_SCHEMA"
	CodeLossy    Code = "E_LOSSY"
	CodeOverflow Code = "E_OVF"
	CodeParam    Code = "E_PARA"
	CodeConflict Code = "E_CONFLICT"
	CodeBound    Code = "E_BOUND"
	CodeState    Code = "E_STATE"
	CodeDiff     Code = "E_DIFF"
	CodeDecode   Code = "E_DECODE"
	CodeOrder    Code = "E_ORDER"
	CodeCapacity Code = "E_CAP"
	CodeSegment  Code = "E_SEG"
	CodeIO       Code = "E_IO"
)

// Loss describes one value that a target schema cannot represent faithfully.
type Loss struct {
	Path        string `json:"path"`
	OldValue    string `json:"old_value"`
	NewValue    string `json:"new_value"`
	Disposition string `json:"disposition"`
	Version     string `json:"version"`
}

// Diff identifies the first observable divergence between two runs.
type Diff struct {
	Window   int    `json:"window"`
	ScreenID string `json:"screen_id,omitempty"`
	Field    string `json:"field"`
	Left     string `json:"left"`
	Right    string `json:"right"`
}

// KernelError is the single structured error type used across schemas, the
// engine, checkpoint recovery, and replay comparison.
type KernelError struct {
	Code            Code     `json:"code"`
	Message         string   `json:"message"`
	Version         string   `json:"version,omitempty"`
	Field           string   `json:"field,omitempty"`
	NativeField     string   `json:"native_field,omitempty"`
	Window          int      `json:"window,omitempty"`
	ScreenID        string   `json:"screen_id,omitempty"`
	RequestedScale  *Scale   `json:"requested_scale,omitempty"`
	AllowedScales   []Scale  `json:"allowed_scales,omitempty"`
	RequestedChroma *Chroma  `json:"requested_chroma,omitempty"`
	AllowedChromas  []Chroma `json:"allowed_chromas,omitempty"`
	Operands        []int64  `json:"operands,omitempty"`
	Actual          *int64   `json:"actual,omitempty"`
	Bound           *int64   `json:"bound,omitempty"`
	State           string   `json:"state,omitempty"`
	TargetState     string   `json:"target_state,omitempty"`
	Progress        int      `json:"progress,omitempty"`
	Losses          []Loss   `json:"losses,omitempty"`
	Diff            *Diff    `json:"diff,omitempty"`
	Cause           string   `json:"cause,omitempty"`
}

func (e *KernelError) Error() string {
	if e == nil {
		return ""
	}
	var b strings.Builder
	b.WriteString(string(e.Code))
	if e.Message != "" {
		b.WriteString(": ")
		b.WriteString(e.Message)
	}
	parts := make([]string, 0, 10)
	if e.Version != "" {
		parts = append(parts, "version="+e.Version)
	}
	if e.Field != "" {
		parts = append(parts, "field="+e.Field)
	}
	if e.NativeField != "" && e.NativeField != e.Field {
		parts = append(parts, "native_field="+e.NativeField)
	}
	if e.Window != 0 {
		parts = append(parts, fmt.Sprintf("window=%d", e.Window))
	}
	if e.ScreenID != "" {
		parts = append(parts, "screen="+e.ScreenID)
	}
	if e.RequestedScale != nil {
		parts = append(parts, fmt.Sprintf("requested_scale=%d", *e.RequestedScale))
	}
	if len(e.AllowedScales) != 0 {
		parts = append(parts, fmt.Sprintf("allowed_scales=%v", e.AllowedScales))
	}
	if len(e.Operands) != 0 {
		parts = append(parts, fmt.Sprintf("operands=%v", e.Operands))
	}
	if e.Actual != nil && e.Bound != nil {
		parts = append(parts, fmt.Sprintf("actual=%d bound=%d", *e.Actual, *e.Bound))
	}
	if e.State != "" {
		parts = append(parts, "state="+e.State)
	}
	if e.TargetState != "" {
		parts = append(parts, "target_state="+e.TargetState)
	}
	if e.Diff != nil {
		parts = append(parts, fmt.Sprintf("diff_window=%d diff_screen=%s diff_field=%s", e.Diff.Window, e.Diff.ScreenID, e.Diff.Field))
	}
	if len(parts) > 0 {
		b.WriteString(" [")
		b.WriteString(strings.Join(parts, ", "))
		b.WriteString("]")
	}
	return b.String()
}

func NewError(code Code, message string) *KernelError {
	return &KernelError{Code: code, Message: message}
}

func HasCode(err error, code Code) bool {
	var ke *KernelError
	return errors.As(err, &ke) && ke.Code == code
}

func AsKernelError(err error) (*KernelError, bool) {
	var ke *KernelError
	if errors.As(err, &ke) {
		return ke, true
	}
	return nil, false
}

func Overflow(field string, a, b int64) *KernelError {
	return &KernelError{
		Code:     CodeOverflow,
		Message:  "integer conversion overflow",
		Field:    field,
		Operands: []int64{a, b},
	}
}
