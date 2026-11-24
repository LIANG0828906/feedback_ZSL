package core

import (
	"errors"
	"fmt"
)

const (
	CodeDecode = "E_DECODE"
	CodeMeta   = "E_META"
	CodePara   = "E_PARA"
	CodeState  = "E_STATE"
	CodeBand   = "E_BAND"
)

// Error is the stable error envelope used across the command line tools, HTTP
// API and tests. Field and Range are populated for validation errors.
type Error struct {
	Code      string `json:"code"`
	Field     string `json:"field,omitempty"`
	Range     string `json:"range,omitempty"`
	TileIndex int    `json:"tile_index,omitempty"`
	Boundary  string `json:"boundary,omitempty"`
	Message   string `json:"message"`
}

func (e *Error) Error() string {
	if e.Field != "" {
		return fmt.Sprintf("%s: %s (%s)", e.Code, e.Message, e.Field)
	}
	return fmt.Sprintf("%s: %s", e.Code, e.Message)
}

func NewError(code, message string) *Error {
	return &Error{Code: code, Message: message}
}

func FieldError(code, field, rng, message string) *Error {
	return &Error{Code: code, Field: field, Range: rng, Message: message}
}

func CodeOf(err error) string {
	var target *Error
	if errors.As(err, &target) {
		return target.Code
	}
	return ""
}

func IsCode(err error, code string) bool {
	return CodeOf(err) == code
}
