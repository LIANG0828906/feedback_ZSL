package textureconv

import (
	"errors"
	"fmt"
	"strings"
)

// Code is a stable machine-readable error code.
type Code string

const (
	CodeSchema  Code = "E_SCHEMA"
	CodeParam   Code = "E_PARA"
	CodeState   Code = "E_STATE"
	CodeDiff    Code = "E_DIFF"
	CodeIO      Code = "E_IO"
	CodeProcess Code = "E_PROCESS"
)

// Error is returned for all expected conversion failures. Entry is an entry
// sequence number (1-based), Tile is a tile number (0-based), and Field names
// the failing JSON field or logical operation.
type Error struct {
	Code     Code
	Entry    int
	Tile     int
	Field    string
	Msg      string
	Pixel    Point
	HasPixel bool
	Left     []uint16
	Right    []uint16
	Err      error
}

func (e *Error) Error() string {
	if e == nil {
		return "<nil>"
	}
	var b strings.Builder
	b.WriteString(string(e.Code))
	if e.Entry > 0 {
		fmt.Fprintf(&b, " entry=%d", e.Entry)
	}
	if e.Tile >= 0 {
		fmt.Fprintf(&b, " tile=%d", e.Tile)
	}
	if e.Field != "" {
		fmt.Fprintf(&b, " field=%s", e.Field)
	}
	if e.HasPixel {
		fmt.Fprintf(&b, " pixel=(%d,%d)", e.Pixel.X, e.Pixel.Y)
	}
	if e.Msg != "" {
		b.WriteString(": ")
		b.WriteString(e.Msg)
	}
	if e.Err != nil {
		b.WriteString(": ")
		b.WriteString(e.Err.Error())
	}
	return b.String()
}

func (e *Error) Unwrap() error { return e.Err }

func newError(code Code, entry, tile int, field, format string, args ...any) *Error {
	return &Error{
		Code:  code,
		Entry: entry,
		Tile:  tile,
		Field: field,
		Msg:   fmt.Sprintf(format, args...),
	}
}

func schemaErr(entry int, field, format string, args ...any) *Error {
	return newError(CodeSchema, entry, -1, field, format, args...)
}

func paramErr(entry int, field, format string, args ...any) *Error {
	return newError(CodeParam, entry, -1, field, format, args...)
}

func stateErr(entry, tile int, format string, args ...any) *Error {
	return newError(CodeState, entry, tile, "state", format, args...)
}

func ioErr(field string, err error) *Error {
	return &Error{Code: CodeIO, Entry: 0, Tile: -1, Field: field, Msg: err.Error(), Err: err}
}

// CodeOf returns the stable code carried by err, if any.
func CodeOf(err error) Code {
	var e *Error
	if errors.As(err, &e) {
		return e.Code
	}
	return ""
}
