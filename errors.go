package spectralrender

import (
	"errors"
	"fmt"
	"strings"
)

// ErrorCode is the stable machine-readable error namespace used by the
// component and the command line examples.
type ErrorCode string

const (
	ErrSchema     ErrorCode = "E_SCHEMA"
	ErrDecode     ErrorCode = "E_DECODE"
	ErrPara       ErrorCode = "E_PARA"
	ErrVer        ErrorCode = "E_VER"
	ErrGamut      ErrorCode = "E_GAMUT"
	ErrBound      ErrorCode = "E_BOUND"
	ErrRender     ErrorCode = "E_RENDER"
	ErrCancel     ErrorCode = "E_CANCEL"
	ErrState      ErrorCode = "E_STATE"
	ErrCheckpoint ErrorCode = "E_CHECKPOINT"
)

// Pixel identifies a rendered pixel for diagnostics.
type Pixel struct {
	X int `json:"x"`
	Y int `json:"y"`
}

// Error is intentionally structured: every failure exposed to callers carries
// enough context to point at a schema path, parameter field, pixel/channel or
// pair of versions.
type Error struct {
	Code     ErrorCode `json:"code"`
	Path     string    `json:"path,omitempty"`
	Field    string    `json:"field,omitempty"`
	Message  string    `json:"message"`
	Pixel    *Pixel    `json:"pixel,omitempty"`
	Channel  string    `json:"channel,omitempty"`
	Tile     int       `json:"tile,omitempty"`
	Versions []uint64  `json:"versions,omitempty"`
	Err      error     `json:"-"`
}

func (e *Error) Error() string {
	if e == nil {
		return "spectralrender: nil error"
	}
	var b strings.Builder
	b.WriteString(string(e.Code))
	if e.Path != "" {
		b.WriteString(" path=")
		b.WriteString(e.Path)
	}
	if e.Field != "" {
		b.WriteString(" field=")
		b.WriteString(e.Field)
	}
	if e.Pixel != nil {
		b.WriteString(fmt.Sprintf(" pixel=(%d,%d)", e.Pixel.X, e.Pixel.Y))
	}
	if e.Channel != "" {
		b.WriteString(" channel=")
		b.WriteString(e.Channel)
	}
	if len(e.Versions) > 0 {
		parts := make([]string, len(e.Versions))
		for i, v := range e.Versions {
			parts[i] = fmt.Sprint(v)
		}
		b.WriteString(" versions=")
		b.WriteString(strings.Join(parts, ","))
	}
	if e.Message != "" {
		b.WriteString(": ")
		b.WriteString(e.Message)
	}
	if e.Err != nil {
		b.WriteString(": ")
		b.WriteString(e.Err.Error())
	}
	return b.String()
}

func (e *Error) Unwrap() error { return e.Err }

func newError(code ErrorCode, path, message string) *Error {
	return &Error{Code: code, Path: path, Message: message}
}

func paraError(field, message string) *Error {
	return &Error{Code: ErrPara, Field: field, Path: field, Message: message}
}

func schemaError(path, message string) *Error {
	return &Error{Code: ErrSchema, Path: path, Message: message}
}

func decodeError(path string, err error) *Error {
	return &Error{Code: ErrDecode, Path: path, Message: "invalid JSON or unreadable data", Err: err}
}

func verError(mine, other uint64, path, message string) *Error {
	return &Error{Code: ErrVer, Path: path, Versions: []uint64{mine, other}, Message: message}
}

func gamutError(p Pixel, channel, message string) *Error {
	return &Error{Code: ErrGamut, Pixel: &p, Channel: channel, Message: message}
}

func AsError(err error) *Error {
	var e *Error
	if errors.As(err, &e) {
		return e
	}
	return nil
}

func CodeOf(err error) ErrorCode {
	if e := AsError(err); e != nil {
		return e.Code
	}
	return ""
}
