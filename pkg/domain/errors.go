package domain

import (
	"errors"
	"fmt"
)

// Code is the stable machine-readable error code exposed by the domain
// component.
type Code string

const (
	ErrSchema   Code = "E_SCHEMA"
	ErrOrder    Code = "E_ORDER"
	ErrRate     Code = "E_RATE"
	ErrPara     Code = "E_PARA"
	ErrOverflow Code = "E_OVF"
	ErrLip      Code = "E_LIP"
	ErrConflict Code = "E_CONFLICT"
	ErrState    Code = "E_STATE"
	ErrDecode   Code = "E_DECODE"
	ErrStage    Code = "E_STAGE"
	ErrDiff     Code = "E_DIFF"
)

// Error carries both a stable code and the diagnostics required to locate the
// failing input. Not every field applies to every code.
type Error struct {
	Code       Code   `json:"code"`
	Path       string `json:"path,omitempty"`
	PhonemeID  string `json:"phoneme_id,omitempty"`
	PhonemeA   string `json:"phoneme_a,omitempty"`
	PhonemeB   string `json:"phoneme_b,omitempty"`
	PackageID  string `json:"package_id,omitempty"`
	DialogueID string `json:"dialogue_id,omitempty"`
	ShardID    string `json:"shard_id,omitempty"`
	Field      string `json:"field,omitempty"`
	Frame      int    `json:"frame,omitempty"`
	HaveFrame  bool   `json:"have_frame,omitempty"`
	LipA       string `json:"lip_a,omitempty"`
	LipB       string `json:"lip_b,omitempty"`
	OperandA   int64  `json:"operand_a,omitempty"`
	OperandB   int64  `json:"operand_b,omitempty"`
	Reason     string `json:"reason,omitempty"`
	Cause      error  `json:"-"`
}

func (e *Error) Error() string {
	if e == nil {
		return "<nil>"
	}
	msg := string(e.Code)
	if e.Path != "" {
		msg += " path=" + e.Path
	}
	if e.DialogueID != "" {
		msg += " dialogue=" + e.DialogueID
	}
	if e.PhonemeID != "" {
		msg += " phoneme=" + e.PhonemeID
	}
	if e.PhonemeA != "" || e.PhonemeB != "" {
		msg += fmt.Sprintf(" phonemes=%s,%s", e.PhonemeA, e.PhonemeB)
	}
	if e.PackageID != "" {
		msg += " package=" + e.PackageID
	}
	if e.ShardID != "" {
		msg += " shard=" + e.ShardID
	}
	if e.Field != "" {
		msg += " field=" + e.Field
	}
	if e.HaveFrame {
		msg += fmt.Sprintf(" frame=%d", e.Frame)
	}
	if e.LipA != "" || e.LipB != "" {
		msg += fmt.Sprintf(" lips=%s->%s", e.LipA, e.LipB)
	}
	if e.OperandA != 0 || e.OperandB != 0 {
		msg += fmt.Sprintf(" operands=(%d,%d)", e.OperandA, e.OperandB)
	}
	if e.Reason != "" {
		msg += " reason=" + e.Reason
	}
	if e.Cause != nil {
		msg += ": " + e.Cause.Error()
	}
	return msg
}

func (e *Error) Unwrap() error { return e.Cause }

func NewError(code Code, path, reason string) *Error {
	return &Error{Code: code, Path: path, Reason: reason}
}

func CodeOf(err error) Code {
	var de *Error
	if errors.As(err, &de) {
		return de.Code
	}
	return ""
}

func IsCode(err error, code Code) bool {
	return CodeOf(err) == code
}
