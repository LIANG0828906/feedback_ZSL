package tex

import (
	"fmt"
	"strings"
)

const (
	CodePara   = "E_PARA"
	CodeLayout = "E_LAYOUT"
	CodeLoss   = "E_LOSS"
	CodeState  = "E_STATE"
	CodeDecode = "E_DECODE"
	CodeDiff   = "E_DIFF"
	CodeCommit = "E_COMMIT"
	CodeBusy   = "E_BUSY"
)

// Error is the stable, machine-readable error contract used by the kernel and
// exposed unchanged by the local HTTP API.
type Error struct {
	Code         string       `json:"code"`
	Message      string       `json:"message"`
	Field        string       `json:"field,omitempty"`
	ChannelIndex *int         `json:"channel_index,omitempty"`
	Dropped      []ChannelRef `json:"dropped_channels,omitempty"`
	Details      any          `json:"details,omitempty"`
}

func (e *Error) Error() string {
	if e == nil {
		return ""
	}
	parts := []string{e.Code}
	if e.Field != "" {
		parts = append(parts, "field="+e.Field)
	}
	if e.ChannelIndex != nil {
		parts = append(parts, fmt.Sprintf("channel_index=%d", *e.ChannelIndex))
	}
	if len(e.Dropped) > 0 {
		names := make([]string, 0, len(e.Dropped))
		for _, ref := range e.Dropped {
			names = append(names, ref.String())
		}
		parts = append(parts, "dropped="+strings.Join(names, ","))
	}
	if e.Message != "" {
		parts = append(parts, e.Message)
	}
	return strings.Join(parts, ": ")
}

func NewError(code, field, message string) *Error {
	return &Error{Code: code, Field: field, Message: message}
}

func ErrPara(field, message string) *Error {
	return NewError(CodePara, field, message)
}

func ErrLayout(field string, channel int, message string) *Error {
	idx := channel
	return &Error{
		Code:         CodeLayout,
		Field:        field,
		ChannelIndex: &idx,
		Message:      message,
	}
}

func ErrLoss(dropped []ChannelRef) *Error {
	return &Error{
		Code:    CodeLoss,
		Message: "conversion is not reversible because source channels were dropped",
		Dropped: dropped,
	}
}

func ErrDecode(message string) *Error {
	return NewError(CodeDecode, "", message)
}

func ErrState(message string) *Error {
	return NewError(CodeState, "", message)
}
