package acoustic

import (
	"errors"
	"fmt"
	"strings"
)

type Code string

const (
	CodeSchema     Code = "E_SCHEMA"
	CodeGeometry   Code = "E_GEO"
	CodeNoChain    Code = "E_NOCHAIN"
	CodeState      Code = "E_STATE"
	CodeCheckpoint Code = "E_CHECKPOINT"
	CodeTruth      Code = "E_TRUTH"
	CodeInternal   Code = "E_INTERNAL"
)

type Error struct {
	Code         Code   `json:"code"`
	Op           string `json:"op,omitempty"`
	Message      string `json:"message"`
	Field        string `json:"field,omitempty"`
	RoomIDs      []int  `json:"roomIds,omitempty"`
	WallIDs      []int  `json:"wallIds,omitempty"`
	OpeningIDs   []int  `json:"openingIds,omitempty"`
	SourceIDs    []int  `json:"sourceIds,omitempty"`
	ListenerIDs  []int  `json:"listenerIds,omitempty"`
	ReflectorIDs []int  `json:"reflectorIds,omitempty"`
	Cause        string `json:"cause,omitempty"`
	CauseError   error  `json:"-"`
}

func (e *Error) Error() string {
	var parts []string
	parts = append(parts, string(e.Code))
	if e.Op != "" {
		parts = append(parts, e.Op)
	}
	if e.Message != "" {
		parts = append(parts, e.Message)
	}
	if e.Field != "" {
		parts = append(parts, "field="+e.Field)
	}
	if len(e.RoomIDs) > 0 {
		parts = append(parts, fmt.Sprintf("rooms=%v", e.RoomIDs))
	}
	if len(e.WallIDs) > 0 {
		parts = append(parts, fmt.Sprintf("walls=%v", e.WallIDs))
	}
	if len(e.OpeningIDs) > 0 {
		parts = append(parts, fmt.Sprintf("openings=%v", e.OpeningIDs))
	}
	if len(e.SourceIDs) > 0 {
		parts = append(parts, fmt.Sprintf("sources=%v", e.SourceIDs))
	}
	if len(e.ListenerIDs) > 0 {
		parts = append(parts, fmt.Sprintf("listeners=%v", e.ListenerIDs))
	}
	if len(e.ReflectorIDs) > 0 {
		parts = append(parts, fmt.Sprintf("reflectors=%v", e.ReflectorIDs))
	}
	if e.Cause != "" {
		parts = append(parts, "cause="+e.Cause)
	}
	return strings.Join(parts, " ")
}

func (e *Error) Is(target error) bool {
	var other *Error
	return errors.As(target, &other) && e.Code == other.Code
}

func (e *Error) Unwrap() error {
	return e.CauseError
}

func NewError(code Code, op, message string) *Error {
	return &Error{Code: code, Op: op, Message: message}
}

func ErrorCode(err error) Code {
	var domainErr *Error
	if errors.As(err, &domainErr) {
		return domainErr.Code
	}
	return ""
}
