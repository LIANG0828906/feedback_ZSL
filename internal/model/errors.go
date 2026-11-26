package model

import (
	"errors"
	"fmt"
)

type Code string

const (
	EDecode  Code = "E_DECODE"
	EPara    Code = "E_PARA"
	EOrder   Code = "E_ORDER"
	EDensity Code = "E_DENS"
	EBudget  Code = "E_BUDGET"
	EBlock   Code = "E_BLOCK"
	EDiverge Code = "E_DIVERGE"
)

type Error struct {
	Code         Code
	Message      string
	RayID        string
	MediumID     string
	CameraID     string
	Field        string
	BlockX       int
	BlockY       int
	HasBlock     bool
	Actual       int
	Limit        int
	FirstPixelX  int
	FirstPixelY  int
	Measurements map[string]float64
	Cause        error
}

func (e *Error) Error() string {
	var where []string
	if e.RayID != "" {
		where = append(where, "ray="+e.RayID)
	}
	if e.MediumID != "" {
		where = append(where, "medium="+e.MediumID)
	}
	if e.CameraID != "" {
		where = append(where, "camera="+e.CameraID)
	}
	if e.Field != "" {
		where = append(where, "field="+e.Field)
	}
	if e.HasBlock {
		where = append(where, fmt.Sprintf("block=(%d,%d)", e.BlockX, e.BlockY))
	}
	if e.Actual != 0 || e.Limit != 0 {
		where = append(where, fmt.Sprintf("actual=%d limit=%d", e.Actual, e.Limit))
	}
	if len(e.Measurements) > 0 {
		where = append(where, fmt.Sprintf("measures=%v", e.Measurements))
	}
	msg := string(e.Code) + ": " + e.Message
	if len(where) > 0 {
		msg += " [" + join(where, ", ") + "]"
	}
	if e.Cause != nil {
		msg += ": " + e.Cause.Error()
	}
	return msg
}

func (e *Error) Unwrap() error { return e.Cause }

func (e *Error) WithCause(cause error) *Error {
	e.Cause = cause
	return e
}

func NewError(code Code, format string, args ...any) *Error {
	return &Error{
		Code: code, Message: fmt.Sprintf(format, args...),
		BlockX: -1, BlockY: -1, FirstPixelX: -1, FirstPixelY: -1,
	}
}

func CodeOf(err error) Code {
	var ke *Error
	if errors.As(err, &ke) {
		return ke.Code
	}
	return ""
}

func withMedium(e *Error, id, field string) *Error {
	e.MediumID = id
	e.Field = field
	return e
}

func DecodeError(format string, args ...any) *Error {
	return NewError(EDecode, format, args...)
}

func ParamError(id, field, format string, args ...any) *Error {
	return withMedium(NewError(EPara, format, args...), id, field)
}

func OrderError(format string, args ...any) *Error {
	return NewError(EOrder, format, args...)
}

func DensityError(id, field, format string, args ...any) *Error {
	return withMedium(NewError(EDensity, format, args...), id, field)
}

func BlockError(x, y int, cause error) *Error {
	e := NewError(EBlock, "block rendering failed")
	e.BlockX, e.BlockY = x, y
	e.HasBlock = true
	e.Cause = cause
	return e
}

func join(xs []string, sep string) string {
	out := ""
	for i, x := range xs {
		if i > 0 {
			out += sep
		}
		out += x
	}
	return out
}
