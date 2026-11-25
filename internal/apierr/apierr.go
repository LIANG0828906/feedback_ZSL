package apierr

import "fmt"

type Code string

const (
	EDecode Code = "E_DECODE"
	ESchema Code = "E_SCHEMA"
	EPara   Code = "E_PARA"
	ELabel  Code = "E_LABEL"
	EStage  Code = "E_STAGE"
	EState  Code = "E_STATE"
	EBand   Code = "E_BAND"
	EBound  Code = "E_BOUND"
)

type Location struct {
	PixelX *int `json:"pixel_x,omitempty"`
	PixelY *int `json:"pixel_y,omitempty"`
	BlockX *int `json:"block_x,omitempty"`
	BlockY *int `json:"block_y,omitempty"`
}

type Error struct {
	Code     Code           `json:"code"`
	Message  string         `json:"message"`
	Field    string         `json:"field,omitempty"`
	Location Location       `json:"location,omitempty"`
	Details  map[string]any `json:"details,omitempty"`
}

func (e *Error) Error() string {
	if e == nil {
		return ""
	}
	return fmt.Sprintf("%s: %s", e.Code, e.Message)
}

func New(code Code, field string, format string, args ...any) *Error {
	return &Error{Code: code, Field: field, Message: fmt.Sprintf(format, args...)}
}

func (e *Error) WithPixel(x, y int) *Error {
	e.Location.PixelX = &x
	e.Location.PixelY = &y
	if e.Details == nil {
		e.Details = map[string]any{}
	}
	e.Details["pixel"] = [2]int{x, y}
	return e
}

func (e *Error) WithBlock(bx, by int) *Error {
	e.Location.BlockX = &bx
	e.Location.BlockY = &by
	if e.Details == nil {
		e.Details = map[string]any{}
	}
	e.Details["block"] = [2]int{bx, by}
	return e
}

func (e *Error) WithDetail(key string, value any) *Error {
	if e.Details == nil {
		e.Details = map[string]any{}
	}
	e.Details[key] = value
	return e
}

func As(err error) (*Error, bool) {
	if err == nil {
		return nil, false
	}
	if ae, ok := err.(*Error); ok {
		return ae, true
	}
	return nil, false
}
