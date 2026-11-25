package errs

import "fmt"

type Code string

const (
	EVersion Code = "E_VERSION"
	ESchema  Code = "E_SCHEMA"
	ECap     Code = "E_CAP"
	EOvf     Code = "E_OVF"
	EPack    Code = "E_PACK"
	EState   Code = "E_STATE"
	EDiff    Code = "E_DIFF"
	EDecode  Code = "E_DECODE"
	EOrder   Code = "E_ORDER"
	EFlag    Code = "E_FLAG"
	EStage   Code = "E_STAGE"
	EHash    Code = "E_HASH"
)

type Error struct {
	Code      Code   `json:"code"`
	Path      string `json:"path,omitempty"`
	Branch    string `json:"branch,omitempty"`
	Channel   int    `json:"channel,omitempty"`
	Packet    int    `json:"packet,omitempty"`
	Window    int    `json:"window,omitempty"`
	Frame     int64  `json:"frame,omitempty"`
	EventID   int64  `json:"eventId,omitempty"`
	RawValue  int64  `json:"rawValue,omitempty"`
	Reason    string `json:"reason,omitempty"`
	First     int64  `json:"firstOffset,omitempty"`
	FirstPack int    `json:"firstPacket,omitempty"`
	Err       error  `json:"-"`
}

func (e *Error) Error() string {
	msg := string(e.Code)
	if e.Reason != "" {
		msg += ": " + e.Reason
	}
	if e.Branch != "" {
		msg += " branch=" + e.Branch
	}
	if e.Path != "" {
		msg += " path=" + e.Path
	}
	if e.Channel >= 0 && (e.Code == EPack || e.Code == EOrder || e.Code == EFlag || e.Code == EStage || e.Code == EState || e.Code == EDiff) {
		msg += fmt.Sprintf(" channel=%d", e.Channel)
	}
	if e.Packet >= 0 && (e.Code == EPack || e.Code == EStage || e.Code == EState || e.Code == EDiff) {
		msg += fmt.Sprintf(" packet=%d", e.Packet)
	}
	if e.Frame > 0 {
		msg += fmt.Sprintf(" frame=%d", e.Frame)
	}
	if e.EventID != 0 {
		msg += fmt.Sprintf(" event=%d", e.EventID)
	}
	if e.RawValue != 0 || e.Code == EOvf {
		msg += fmt.Sprintf(" raw=%d", e.RawValue)
	}
	if e.Err != nil {
		msg += ": " + e.Err.Error()
	}
	return msg
}

func (e *Error) Unwrap() error { return e.Err }

func New(code Code, reason string) *Error {
	return &Error{Code: code, Reason: reason, Channel: -1, Packet: -1}
}

func Wrap(code Code, reason string, err error) *Error {
	return &Error{Code: code, Reason: reason, Err: err, Channel: -1, Packet: -1}
}
