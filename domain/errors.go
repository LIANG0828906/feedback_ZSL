package domain

import (
	"errors"
	"fmt"
	"strings"
)

// Code is a stable machine-readable error code for the lip-sync kernel.
type Code string

const (
	CodeSchema   Code = "E_SCHEMA"
	CodeLossy    Code = "E_LOSSY"
	CodeOverflow Code = "E_OVF"
	CodeParam    Code = "E_PARA"
	CodeBudget   Code = "E_BUDGET"
	CodeState    Code = "E_STATE"
	CodeDiff     Code = "E_DIFF"
	CodeOrder    Code = "E_ORDER"
	CodeRate     Code = "E_RATE"
	CodeClass    Code = "E_CLASS"
	CodeStage    Code = "E_STAGE"
	CodeCanceled Code = "E_CANCELED"
)

// Error carries enough context for callers to locate the offending source
// field, shard, frame, or phoneme without parsing a free-form message.
type Error struct {
	Code       Code   `json:"code"`
	Message    string `json:"message"`
	Path       string `json:"path,omitempty"`
	Version    string `json:"source_version,omitempty"`
	Field      string `json:"field,omitempty"`
	DialogueID string `json:"dialogue_id,omitempty"`
	ShardIndex int    `json:"shard_index,omitempty"`
	PhonemeID  string `json:"phoneme_id,omitempty"`
	Frame      int64  `json:"frame,omitempty"`
	Channel    int    `json:"channel,omitempty"`
	OperandA   int64  `json:"operand_a,omitempty"`
	OperandB   int64  `json:"operand_b,omitempty"`
	Value      string `json:"value,omitempty"`
	Expected   string `json:"expected,omitempty"`
	Actual     string `json:"actual,omitempty"`
	Cause      error  `json:"-"`
}

func (e *Error) Error() string {
	if e == nil {
		return ""
	}
	var b strings.Builder
	b.WriteString(string(e.Code))
	if e.Version != "" {
		fmt.Fprintf(&b, " version=%s", e.Version)
	}
	if e.ShardIndex != 0 || e.DialogueID != "" {
		fmt.Fprintf(&b, " shard=%d dialogue=%s", e.ShardIndex, e.DialogueID)
	}
	frameSet := e.Frame != 0 || strings.Contains(e.Field, "frame")
	if frameSet {
		fmt.Fprintf(&b, " frame=%d", e.Frame)
	}
	channelSet := e.Channel != 0 || strings.Contains(e.Field, "channel")
	if channelSet {
		fmt.Fprintf(&b, " channel=%d", e.Channel)
	}
	if e.PhonemeID != "" {
		fmt.Fprintf(&b, " phoneme=%s", e.PhonemeID)
	}
	if e.Path != "" {
		fmt.Fprintf(&b, " path=%s", e.Path)
	}
	if e.Field != "" {
		fmt.Fprintf(&b, " field=%s", e.Field)
	}
	if e.OperandA != 0 || e.OperandB != 0 {
		fmt.Fprintf(&b, " operands=(%d,%d)", e.OperandA, e.OperandB)
	}
	if e.Value != "" {
		fmt.Fprintf(&b, " value=%s", e.Value)
	}
	if e.Expected != "" || e.Actual != "" {
		fmt.Fprintf(&b, " expected=%q actual=%q", e.Expected, e.Actual)
	}
	if e.Message != "" {
		fmt.Fprintf(&b, ": %s", e.Message)
	}
	if e.Cause != nil {
		fmt.Fprintf(&b, ": %v", e.Cause)
	}
	return b.String()
}

func (e *Error) Unwrap() error { return e.Cause }

func NewError(code Code, message string) *Error {
	return &Error{Code: code, Message: message}
}

func ErrorCode(err error) Code {
	var de *Error
	if errors.As(err, &de) {
		return de.Code
	}
	return ""
}

func AsError(err error) (*Error, bool) {
	var de *Error
	if errors.As(err, &de) {
		return de, true
	}
	return nil, false
}

// Annotate adds missing location details without changing the stable code.
func Annotate(err error, mut func(*Error)) error {
	de, ok := AsError(err)
	if !ok {
		return err
	}
	cp := *de
	mut(&cp)
	return &cp
}
