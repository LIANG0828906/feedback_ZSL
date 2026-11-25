package fog

import (
	"errors"
	"fmt"
	"strings"
)

// ErrorCode is the stable diagnostic contract used by the kernel and the
// workbench. New codes should be added only when a caller needs to branch on
// them.
type ErrorCode string

const (
	EDecode   ErrorCode = "E_DECODE"
	EPara     ErrorCode = "E_PARA"
	EOrder    ErrorCode = "E_ORDER"
	EDens     ErrorCode = "E_DENS"
	EBlock    ErrorCode = "E_BLOCK"
	EDiverge  ErrorCode = "E_DIVERGE"
	EBudget   ErrorCode = "E_BUDGET"
	ECache    ErrorCode = "E_CACHE"
	ECheckpt  ErrorCode = "E_CHECKPOINT"
	ECanceled ErrorCode = "E_CANCELED"
)

// Fault is deliberately structured rather than a formatted string only. This
// lets the HTTP diagnostics endpoint and integration tests inspect the exact
// field path and measured value.
type Fault struct {
	Code      ErrorCode `json:"code"`
	Path      string    `json:"path,omitempty"`
	MediaID   string    `json:"media_id,omitempty"`
	TileID    string    `json:"tile_id,omitempty"`
	RayID     string    `json:"ray_id,omitempty"`
	Pixel     *PixelRef `json:"pixel,omitempty"`
	Actual    any       `json:"actual,omitempty"`
	Expected  any       `json:"expected,omitempty"`
	Message   string    `json:"message"`
	Cause     string    `json:"cause,omitempty"`
	Repeating bool      `json:"repeating,omitempty"`
}

type PixelRef struct {
	X int `json:"x"`
	Y int `json:"y"`
}

func (f *Fault) Error() string {
	if f == nil {
		return "<nil>"
	}
	var b strings.Builder
	b.WriteString(string(f.Code))
	if f.Path != "" {
		b.WriteString(" path=")
		b.WriteString(f.Path)
	}
	if f.MediaID != "" {
		b.WriteString(" media=")
		b.WriteString(f.MediaID)
	}
	if f.TileID != "" {
		b.WriteString(" tile=")
		b.WriteString(f.TileID)
	}
	if f.Pixel != nil {
		fmt.Fprintf(&b, " pixel=(%d,%d)", f.Pixel.X, f.Pixel.Y)
	}
	if f.Actual != nil {
		fmt.Fprintf(&b, " actual=%v", f.Actual)
	}
	if f.Expected != nil {
		fmt.Fprintf(&b, " expected=%v", f.Expected)
	}
	if f.Message != "" {
		b.WriteString(": ")
		b.WriteString(f.Message)
	}
	if f.Cause != "" {
		b.WriteString("; cause=")
		b.WriteString(f.Cause)
	}
	return b.String()
}

func FaultAs(err error) (*Fault, bool) {
	var f *Fault
	if errors.As(err, &f) {
		return f, true
	}
	return nil, false
}

func newFault(code ErrorCode, path, message string, actual, expected any) *Fault {
	return &Fault{Code: code, Path: path, Message: message, Actual: actual, Expected: expected}
}

// FaultList is returned when multiple independent blocks failed. A failed
// block is represented as data, allowing the surviving blocks to be committed.
type FaultList []*Fault

func (l FaultList) Error() string {
	if len(l) == 0 {
		return ""
	}
	var b strings.Builder
	fmt.Fprintf(&b, "%d fault(s): ", len(l))
	for i, f := range l {
		if i > 0 {
			b.WriteString("; ")
		}
		b.WriteString(f.Error())
	}
	return b.String()
}

func (l FaultList) Unwrap() []error {
	out := make([]error, 0, len(l))
	for _, f := range l {
		if f != nil {
			out = append(out, f)
		}
	}
	return out
}
