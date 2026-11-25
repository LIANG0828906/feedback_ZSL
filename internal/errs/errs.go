// Package errs defines the stable error envelope used by the CLI, HTTP API,
// renderer, and verifier.
package errs

import (
	"encoding/json"
	"errors"
	"fmt"
)

// Code is a stable machine-readable error code.
type Code string

const (
	CodeSchema  Code = "E_SCHEMA"
	CodePara    Code = "E_PARA"
	CodeDecode  Code = "E_DECODE"
	CodeSeam    Code = "E_SEAM"
	CodeQMix    Code = "E_QMIX"
	CodeVersion Code = "E_VER"
	CodeGamut   Code = "E_GAMUT"
	CodeBudget  Code = "E_BUDGET"
	CodePaused  Code = "E_PAUSED"
	CodeCancel  Code = "E_CANCELED"
	CodeIO      Code = "E_IO"
	CodeVerify  Code = "E_VERIFY"
)

// Error is both an internal error and the public API error object.
type Error struct {
	Code            Code     `json:"code"`
	Field           string   `json:"field,omitempty"`
	Message         string   `json:"message"`
	Range           string   `json:"range,omitempty"`
	X               *int     `json:"x,omitempty"`
	Y               *int     `json:"y,omitempty"`
	Channel         string   `json:"channel,omitempty"`
	Tile            *int     `json:"tile,omitempty"`
	AdjacentTile    *int     `json:"adjacent_tile,omitempty"`
	Jump            *float64 `json:"jump,omitempty"`
	ExpectedVersion string   `json:"expected_version,omitempty"`
	ActualVersion   string   `json:"actual_version,omitempty"`
	Wrapped         string   `json:"wrapped,omitempty"`
}

func (e *Error) Error() string {
	b, _ := json.Marshal(e)
	return string(b)
}

func (e *Error) Unwrap() error {
	if e.Wrapped == "" {
		return nil
	}
	return errors.New(e.Wrapped)
}

func New(code Code, format string, args ...any) *Error {
	return &Error{Code: code, Message: fmt.Sprintf(format, args...)}
}

func Schema(field, format string, args ...any) *Error {
	return &Error{Code: CodeSchema, Field: field, Message: fmt.Sprintf(format, args...)}
}

func Para(field, valueRange, format string, args ...any) *Error {
	return &Error{
		Code:    CodePara,
		Field:   field,
		Range:   valueRange,
		Message: fmt.Sprintf(format, args...),
	}
}

func Decode(field, format string, args ...any) *Error {
	return &Error{Code: CodeDecode, Field: field, Message: fmt.Sprintf(format, args...)}
}

func IO(field string, err error) *Error {
	return &Error{
		Code:    CodeIO,
		Field:   field,
		Message: err.Error(),
		Wrapped: err.Error(),
	}
}

func Gamut(x, y int, channel, format string, args ...any) *Error {
	return &Error{
		Code:    CodeGamut,
		X:       IntPtr(x),
		Y:       IntPtr(y),
		Channel: channel,
		Message: fmt.Sprintf(format, args...),
	}
}

func Version(expected, actual string) *Error {
	return &Error{
		Code:            CodeVersion,
		ExpectedVersion: expected,
		ActualVersion:   actual,
		Message:         fmt.Sprintf("version mismatch: expected %s, got %s", expected, actual),
	}
}

func Seam(tile, adjacent int, jump float64) *Error {
	return &Error{
		Code:         CodeSeam,
		Tile:         IntPtr(tile),
		AdjacentTile: IntPtr(adjacent),
		Jump:         Float64Ptr(jump),
		Message:      fmt.Sprintf("tile boundary luminance jump %.8f exceeds threshold", jump),
	}
}

func QMix(field, format string, args ...any) *Error {
	return &Error{Code: CodeQMix, Field: field, Message: fmt.Sprintf(format, args...)}
}

func As(err error) (*Error, bool) {
	var target *Error
	if errors.As(err, &target) {
		return target, true
	}
	return nil, false
}

func IntPtr(v int) *int             { return &v }
func Float64Ptr(v float64) *float64 { return &v }
