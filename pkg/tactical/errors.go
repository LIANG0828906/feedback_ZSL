package tactical

import (
	"errors"
	"fmt"
)

// Code is a stable machine-readable failure code used by the workbench API.
type Code string

const (
	CodeSchema Code = "E_SCHEMA"
	CodeHeur   Code = "E_HEUR"
	CodeForm   Code = "E_FORM"
	CodePara   Code = "E_PARA"
	CodeTrace  Code = "E_TRACE"
	CodeDiff   Code = "E_DIFF"
)

// Error carries enough context for the UI to point at the exact failing
// field, member, tick, or comparison item.
type Error struct {
	Code          Code          `json:"code"`
	Message       string        `json:"message"`
	Field         string        `json:"field,omitempty"`
	MemberID      string        `json:"member_id,omitempty"`
	Pair          [2]string     `json:"member_pair,omitempty"`
	Tick          int           `json:"tick,omitempty"`
	Item          string        `json:"item,omitempty"`
	DistanceMilli int64         `json:"distance_milli,omitempty"`
	Interval      *TickInterval `json:"interval,omitempty"`
}

func (e *Error) Error() string {
	msg := string(e.Code) + ": " + e.Message
	if e.Field != "" {
		msg += " (field=" + e.Field + ")"
	}
	if e.MemberID != "" {
		msg += " (member=" + e.MemberID + ")"
	}
	if e.Pair[0] != "" || e.Pair[1] != "" {
		msg += fmt.Sprintf(" (members=%s,%s)", e.Pair[0], e.Pair[1])
	}
	if e.Tick != 0 {
		msg += fmt.Sprintf(" (tick=%d)", e.Tick)
	}
	if e.Item != "" {
		msg += " (item=" + e.Item + ")"
	}
	if e.DistanceMilli != 0 {
		msg += fmt.Sprintf(" (distance_milli=%d)", e.DistanceMilli)
	}
	return msg
}

func newError(code Code, message string) *Error {
	return &Error{Code: code, Message: message}
}

func schemaError(field, message string) *Error {
	return &Error{Code: CodeSchema, Field: field, Message: message}
}

func memberError(code Code, memberID, message string) *Error {
	return &Error{Code: code, MemberID: memberID, Message: message}
}

// CodeOf returns the stable code for an error or an empty code.
func CodeOf(err error) Code {
	var target *Error
	if errors.As(err, &target) {
		return target.Code
	}
	return ""
}

// AsError returns the typed workbench error when available.
func AsError(err error) (*Error, bool) {
	var target *Error
	if errors.As(err, &target) {
		return target, true
	}
	return nil, false
}
