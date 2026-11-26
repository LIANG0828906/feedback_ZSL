package mirror

import (
	"fmt"
	"strings"
)

// Stable error codes used by the public API.
const (
	CodeSchema      = "E_SCHEMA"
	CodeLossy       = "E_LOSSY"
	CodeOverflow    = "E_OVF"
	CodeCapacity    = "E_CAP"
	CodeArbitration = "E_ARB"
	CodePacket      = "E_PACK"
	CodeState       = "E_STATE"
	CodeDiff        = "E_DIFF"
)

// Error is the single error type returned by all validation and execution
// paths. The context fields are intentionally data, not formatted strings, so
// callers can make decisions without parsing messages.
type Error struct {
	Code      string
	Path      string
	Message   string
	ScreenID  string
	WindowID  string
	Field     string
	Tick      int64
	Operands  []int64
	Requested string
	Supported []string
	Events    []string
	PacketSeq int
	Measured  int64
	Limit     int64
	Losses    []Loss
}

// Loss describes one non-representable value during a lossy export.
type Loss struct {
	Path        string `json:"path"`
	Value       string `json:"value"`
	Reason      string `json:"reason"`
	Disposition string `json:"disposition"`
}

func (e *Error) Error() string {
	if e == nil {
		return "<nil>"
	}
	var b strings.Builder
	b.WriteString(e.Code)
	if e.Path != "" {
		b.WriteString(" path=")
		b.WriteString(e.Path)
	}
	if e.ScreenID != "" {
		b.WriteString(" screen=")
		b.WriteString(e.ScreenID)
	}
	if e.WindowID != "" {
		b.WriteString(" window=")
		b.WriteString(e.WindowID)
	}
	if e.Field != "" {
		b.WriteString(" field=")
		b.WriteString(e.Field)
	}
	if e.Tick != 0 || len(e.Events) > 0 {
		fmt.Fprintf(&b, " tick=%d", e.Tick)
	}
	if len(e.Operands) > 0 {
		fmt.Fprintf(&b, " operands=%v", e.Operands)
	}
	if e.Requested != "" {
		b.WriteString(" requested=")
		b.WriteString(e.Requested)
	}
	if len(e.Supported) > 0 {
		b.WriteString(" supported=")
		b.WriteString(strings.Join(e.Supported, ","))
	}
	if len(e.Events) > 0 {
		b.WriteString(" events=")
		b.WriteString(strings.Join(e.Events, ","))
	}
	if e.PacketSeq != 0 {
		fmt.Fprintf(&b, " packet=%d", e.PacketSeq)
	}
	if e.Measured != 0 || e.Limit != 0 {
		fmt.Fprintf(&b, " measured=%d limit=%d", e.Measured, e.Limit)
	}
	if len(e.Losses) > 0 {
		parts := make([]string, 0, len(e.Losses))
		for _, loss := range e.Losses {
			parts = append(parts, loss.Path+"="+loss.Value+" ("+loss.Reason+")")
		}
		b.WriteString(" losses=[")
		b.WriteString(strings.Join(parts, "; "))
		b.WriteString("]")
	}
	if e.Message != "" {
		b.WriteString(": ")
		b.WriteString(e.Message)
	}
	return b.String()
}

func errf(code, path, format string, args ...any) *Error {
	return &Error{Code: code, Path: path, Message: fmt.Sprintf(format, args...)}
}

// IsCode reports whether err or any wrapped error has the requested stable
// error code.
func IsCode(err error, code string) bool {
	e, ok := AsError(err)
	return ok && e.Code == code
}

// AsError returns the first *Error in an error chain.
func AsError(err error) (*Error, bool) {
	for err != nil {
		if e, ok := err.(*Error); ok {
			return e, true
		}
		type unwrapper interface{ Unwrap() error }
		u, ok := err.(unwrapper)
		if !ok {
			return nil, false
		}
		err = u.Unwrap()
	}
	return nil, false
}
