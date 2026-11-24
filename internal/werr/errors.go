package werr

import "fmt"

const (
	CodeDecode = "E_DECODE"
	CodeRate   = "E_RATE"
	CodePara   = "E_PARA"
	CodeLayout = "E_LAYOUT"
	CodeErr    = "E_ERR"
	CodeState  = "E_STATE"
	CodeFrozen = "E_FROZEN"
	CodeDiff   = "E_DIFF"
	CodeChan   = "E_CHAN"
	CodeOrder  = "E_ORDER"
	CodeClip   = "E_CLIP"
	CodeFrame  = "E_FRAME"
)

// Error is a machine-readable workbench error. It deliberately carries both a
// stable code and the diagnostic context required by the UI and checkpoint
// layers.
type Error struct {
	Code     string
	Message  string
	Field    string
	Frame    int
	Channel  string
	Item     string
	Actual   any
	Expected any
	Min      *float64
	Max      *float64
	Left     any
	Right    any
}

func (e *Error) Error() string {
	if e == nil {
		return ""
	}
	msg := e.Code + ": " + e.Message
	if e.Field != "" {
		msg += " [field=" + e.Field + "]"
	}
	if e.Frame >= 0 {
		msg += fmt.Sprintf(" [frame=%d]", e.Frame)
	}
	if e.Channel != "" {
		msg += " [channel=" + e.Channel + "]"
	}
	if e.Item != "" {
		msg += " [item=" + e.Item + "]"
	}
	return msg
}

func New(code, format string, args ...any) *Error {
	return &Error{Code: code, Message: fmt.Sprintf(format, args...), Frame: -1}
}

func Decode(format string, args ...any) *Error {
	return New(CodeDecode, format, args...)
}

func Para(format string, args ...any) *Error {
	return New(CodePara, format, args...)
}

func Frame(code string, frame int, channel, format string, args ...any) *Error {
	e := New(code, format, args...)
	e.Frame = frame
	e.Channel = channel
	return e
}

func IsCode(err error, code string) bool {
	e, ok := err.(*Error)
	return ok && e.Code == code
}

func As(err error) (*Error, bool) {
	e, ok := err.(*Error)
	return e, ok
}
