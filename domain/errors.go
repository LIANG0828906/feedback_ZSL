package domain

import (
	"errors"
	"fmt"
)

// Code is the stable machine-readable failure class exposed by the kernel.
type Code string

const (
	CodeSchema   Code = "E_SCHEMA"
	CodeLossy    Code = "E_LOSSY"
	CodeOverflow Code = "E_OVF"
	CodeResume   Code = "E_RESUME"
	CodeState    Code = "E_STATE"
	CodeDecode   Code = "E_DECODE"
	CodeOrder    Code = "E_ORDER"
	CodeCapacity Code = "E_CAP"
)

// Error carries both a stable code and the field/path needed for diagnosis.
type Error struct {
	Code      Code   `json:"code"`
	Path      string `json:"path,omitempty"`
	ChannelID string `json:"channel_id,omitempty"`
	PacketID  string `json:"packet_id,omitempty"`
	Field     string `json:"field,omitempty"`
	OperandA  int64  `json:"operand_a,omitempty"`
	OperandB  int64  `json:"operand_b,omitempty"`
	Available int64  `json:"available_frames,omitempty"`
	Required  int64  `json:"required_frames,omitempty"`
	State     string `json:"state,omitempty"`
	Message   string `json:"message"`
}

func (e *Error) Error() string {
	if e == nil {
		return ""
	}
	where := e.Path
	if where == "" {
		where = e.Field
	}
	if where == "" {
		where = "-"
	}
	extra := ""
	if e.ChannelID != "" {
		extra += " channel=" + e.ChannelID
	}
	if e.PacketID != "" {
		extra += " packet=" + e.PacketID
	}
	return fmt.Sprintf("%s path=%s%s: %s", e.Code, where, extra, e.Message)
}

func NewError(code Code, path, message string) *Error {
	return &Error{Code: code, Path: path, Message: message}
}

func CodeOf(err error) Code {
	var domainErr *Error
	if errors.As(err, &domainErr) {
		return domainErr.Code
	}
	return ""
}

func IsCode(err error, code Code) bool {
	return CodeOf(err) == code
}
