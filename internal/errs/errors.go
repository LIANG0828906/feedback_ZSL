// Package errs defines the stable error codes used across the audio pipeline.
package errs

import (
	"errors"
	"fmt"
	"time"
)

type Code string

const (
	EVersion    Code = "E_VERSION"
	EOver       Code = "E_OVER"
	EPack       Code = "E_PACK"
	EState      Code = "E_STATE"
	EDiff       Code = "E_DIFF"
	EMiss       Code = "E_MISS"
	EDup        Code = "E_DUP"
	ETimeout    Code = "E_TIMEOUT"
	ECancel     Code = "E_CANCEL"
	ETerminated Code = "E_TERMINATED"
	ESnapshot   Code = "E_SNAPSHOT"
)

// Error carries enough context for both tests and the diagnostic UI.
type Error struct {
	Code         Code      `json:"code"`
	Message      string    `json:"message"`
	Branch       string    `json:"branch,omitempty"`
	Field        string    `json:"field,omitempty"`
	EventID      string    `json:"event_id,omitempty"`
	PacketSeq    int       `json:"packet_seq"`
	Channel      int       `json:"channel"`
	SampleOffset int64     `json:"sample_offset"`
	FirstSeen    time.Time `json:"first_seen,omitempty"`
	Err          error     `json:"-"`
}

func (e *Error) Error() string {
	if e == nil {
		return "<nil>"
	}
	msg := string(e.Code)
	if e.Message != "" {
		msg += ": " + e.Message
	}
	if e.Branch != "" {
		msg += fmt.Sprintf(" [branch=%s]", e.Branch)
	}
	if e.Field != "" {
		msg += fmt.Sprintf(" [field=%s]", e.Field)
	}
	if e.EventID != "" {
		msg += fmt.Sprintf(" [event=%s]", e.EventID)
	}
	if e.PacketSeq != 0 || e.Channel != 0 {
		msg += fmt.Sprintf(" [channel=%d packet=%d]", e.Channel, e.PacketSeq)
	}
	if e.SampleOffset != 0 {
		msg += fmt.Sprintf(" [sample=%d]", e.SampleOffset)
	}
	return msg
}

func (e *Error) Unwrap() error { return e.Err }

func New(code Code, format string, args ...any) *Error {
	return &Error{Code: code, Message: fmt.Sprintf(format, args...)}
}

func Wrap(code Code, err error, format string, args ...any) *Error {
	return &Error{Code: code, Message: fmt.Sprintf(format, args...), Err: err}
}

func IsCode(err error, code Code) bool {
	var coded *Error
	return errors.As(err, &coded) && coded.Code == code
}

func BranchField(branch, field, format string, args ...any) *Error {
	return &Error{
		Code:    EVersion,
		Message: fmt.Sprintf(format, args...),
		Branch:  branch,
		Field:   field,
	}
}

func Overflow(eventID, field, format string, args ...any) *Error {
	return &Error{
		Code:    EOver,
		Message: fmt.Sprintf(format, args...),
		EventID: eventID,
		Field:   field,
	}
}

func Packet(seq, channel int, field, format string, args ...any) *Error {
	return &Error{
		Code:      EPack,
		Message:   fmt.Sprintf(format, args...),
		PacketSeq: seq,
		Channel:   channel,
		Field:     field,
	}
}
