package e

import "fmt"

// Code is the stable business error code used by the workbench.
type Code string

const (
	Decode Code = "E_DECODE"
	Lossy  Code = "E_LOSSY"
	Ovf    Code = "E_OVF"
	Para   Code = "E_PARA"
	Drift  Code = "E_DRIFT"
	Ver    Code = "E_VER"
	Diff   Code = "E_DIFF"
)

// Error is the coded error envelope. Fields are optional so that each code can
// expose the diagnostics required by the technical specification.
type Error struct {
	Code     Code    `json:"code"`
	Message  string  `json:"message"`
	Version  string  `json:"version,omitempty"`
	Field    string  `json:"field,omitempty"`
	Path     string  `json:"path,omitempty"`
	Block    string  `json:"block,omitempty"`
	Band     string  `json:"band,omitempty"`
	Channel  string  `json:"channel,omitempty"`
	Observed int64   `json:"observed,omitempty"`
	Bound    int64   `json:"bound,omitempty"`
	Operands []int64 `json:"operands,omitempty"`
	Expected string  `json:"expected,omitempty"`
	Actual   string  `json:"actual,omitempty"`
	Detail   string  `json:"detail,omitempty"`
}

func (e *Error) Error() string {
	msg := string(e.Code)
	if e.Field != "" {
		msg += " field=" + e.Field
	}
	if e.Path != "" && e.Path != e.Field {
		msg += " path=" + e.Path
	}
	if e.Block != "" {
		msg += " block=" + e.Block
	}
	if e.Band != "" {
		msg += " band=" + e.Band
	}
	if e.Message != "" {
		msg += ": " + e.Message
	}
	return msg
}

func New(code Code, format string, args ...any) *Error {
	return &Error{Code: code, Message: fmt.Sprintf(format, args...)}
}

func DecodeError(version, path, format string, args ...any) *Error {
	return &Error{
		Code:    Decode,
		Version: version,
		Path:    path,
		Field:   path,
		Message: fmt.Sprintf(format, args...),
	}
}

func Overflow(field string, operands ...int64) *Error {
	return &Error{
		Code:     Ovf,
		Field:    field,
		Path:     field,
		Operands: append([]int64(nil), operands...),
		Message:  "integer conversion overflow",
	}
}

func Param(field, validRange, format string, args ...any) *Error {
	return &Error{
		Code:    Para,
		Field:   field,
		Path:    field,
		Detail:  "valid range " + validRange,
		Message: fmt.Sprintf(format, args...),
	}
}

func As(err error) (*Error, bool) {
	if err == nil {
		return nil, false
	}
	var coded *Error
	for err != nil {
		if ce, ok := err.(*Error); ok {
			coded = ce
			break
		}
		type unwrapper interface{ Unwrap() error }
		u, ok := err.(unwrapper)
		if !ok {
			break
		}
		err = u.Unwrap()
	}
	if coded == nil {
		return nil, false
	}
	return coded, true
}
