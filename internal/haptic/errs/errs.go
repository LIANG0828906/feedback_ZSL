package errs

import (
	"errors"
	"fmt"
)

type Code string

const (
	EVersion Code = "E_VERSION"
	EOver    Code = "E_OVER"
	EFlag    Code = "E_FLAG"
	EDecode  Code = "E_DECODE"
	EOrder   Code = "E_ORDER"
	EState   Code = "E_STATE"
	EStage   Code = "E_STAGE"
	EDiff    Code = "E_DIFF"
	ELossy   Code = "E_LOSSY"
	EHash    Code = "E_HASH"
)

type Error struct {
	Code    Code           `json:"code"`
	Message string         `json:"message"`
	Path    string         `json:"path,omitempty"`
	FrameNo int            `json:"frame_no,omitempty"`
	Channel string         `json:"channel,omitempty"`
	Field   string         `json:"field,omitempty"`
	Seq     int            `json:"packet_seq,omitempty"`
	Details map[string]any `json:"details,omitempty"`
	Cause   error          `json:"-"`
}

func (e *Error) Error() string {
	msg := string(e.Code)
	if e.Message != "" {
		msg += ": " + e.Message
	}
	if e.Path != "" {
		msg += " path=" + e.Path
	}
	if e.FrameNo != 0 {
		msg += fmt.Sprintf(" frame=%d", e.FrameNo)
	}
	if e.Channel != "" {
		msg += " channel=" + e.Channel
	}
	if e.Seq != 0 {
		msg += fmt.Sprintf(" packet_seq=%d", e.Seq)
	}
	if e.Field != "" {
		msg += " field=" + e.Field
	}
	if e.Cause != nil {
		msg += ": " + e.Cause.Error()
	}
	return msg
}

func (e *Error) Unwrap() error { return e.Cause }

func New(code Code, message string) *Error {
	return &Error{Code: code, Message: message}
}

func (e *Error) WithPath(path string) *Error {
	e.Path = path
	return e
}

func (e *Error) WithFrameChannel(frame int, channel string) *Error {
	e.FrameNo = frame
	e.Channel = channel
	return e
}

func Wrap(code Code, message string, cause error) *Error {
	return &Error{Code: code, Message: message, Cause: cause}
}

func WithPath(err error, path string) error {
	var he *Error
	if errors.As(err, &he) && he.Path == "" {
		he.Path = path
	}
	return err
}

func WithContext(err error, frame int, channel string) error {
	var he *Error
	if errors.As(err, &he) {
		if he.FrameNo == 0 {
			he.FrameNo = frame
		}
		if he.Channel == "" {
			he.Channel = channel
		}
	}
	return err
}

func As(err error) (*Error, bool) {
	var he *Error
	ok := errors.As(err, &he)
	return he, ok
}

func CodeOf(err error) Code {
	if he, ok := As(err); ok {
		return he.Code
	}
	return ""
}
