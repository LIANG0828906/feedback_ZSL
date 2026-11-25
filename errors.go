package fog

import (
	"errors"
	"fmt"
)

const (
	CodeSchema  = "E_SCHEMA"
	CodeSeg     = "E_SEG"
	CodeIter    = "E_ITER"
	CodeDiverge = "E_DIVERGE"
	CodeDiff    = "E_DIFF"
	CodeDecode  = "E_DECODE"
	CodeOrder   = "E_ORDER"
	CodeDensity = "E_DENS"
	CodeBlock   = "E_BLOCK"
)

type TileCoord struct {
	X int `json:"x"`
	Y int `json:"y"`
}

type PixelCoord struct {
	X int `json:"x"`
	Y int `json:"y"`
}

type Error struct {
	Code        string      `json:"code"`
	Message     string      `json:"message"`
	Field       string      `json:"field,omitempty"`
	Value       any         `json:"value,omitempty"`
	RayID       string      `json:"ray_id,omitempty"`
	MediumID    string      `json:"medium_id,omitempty"`
	MediumPair  [2]string   `json:"medium_pair,omitempty"`
	Tile        *TileCoord  `json:"tile,omitempty"`
	Pixel       *PixelCoord `json:"pixel,omitempty"`
	Deviation   float64     `json:"deviation,omitempty"`
	Differences [3]float64  `json:"differences,omitempty"`
	FieldName   string      `json:"field_name,omitempty"`
}

func (e *Error) Error() string {
	if e == nil {
		return "<nil>"
	}
	msg := fmt.Sprintf("%s: %s", e.Code, e.Message)
	if e.Field != "" {
		msg += fmt.Sprintf(" field=%s value=%v", e.Field, e.Value)
	}
	if e.RayID != "" {
		msg += " ray=" + e.RayID
	}
	if e.MediumID != "" {
		msg += " medium=" + e.MediumID
	}
	if e.Tile != nil {
		msg += fmt.Sprintf(" tile=(%d,%d)", e.Tile.X, e.Tile.Y)
	}
	if e.Pixel != nil {
		msg += fmt.Sprintf(" pixel=(%d,%d)", e.Pixel.X, e.Pixel.Y)
	}
	if e.Deviation != 0 {
		msg += fmt.Sprintf(" deviation=%.8g", e.Deviation)
	}
	return msg
}

func (e *Error) Is(target error) bool {
	var te *Error
	if errors.As(target, &te) {
		return te.Code == e.Code
	}
	return false
}

func newError(code, format string, args ...any) *Error {
	return &Error{Code: code, Message: fmt.Sprintf(format, args...)}
}

func schemaError(field string, value any, format string, args ...any) *Error {
	e := newError(CodeSchema, format, args...)
	e.Field = field
	e.Value = value
	return e
}

func AsError(err error) *Error {
	var e *Error
	if errors.As(err, &e) {
		return e
	}
	return nil
}
