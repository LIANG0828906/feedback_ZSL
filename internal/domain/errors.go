package domain

import (
	"encoding/json"
	"fmt"
)

const (
	CodeDecode = "E_DECODE"
	CodeMeta   = "E_META"
	CodePara   = "E_PARA"
	CodeDet    = "E_DET"
	CodeTmpl   = "E_TMPL"
	CodeState  = "E_STATE"
	CodeBand   = "E_BAND"
	CodeRead   = "E_READ"
	CodeDiff   = "E_DIFF"
	CodeStore  = "E_STORE"
)

// AppError is the structured error contract shared by the core, HTTP lab,
// Wails bindings, and the web UI.
type AppError struct {
	Code          string `json:"code"`
	Message       string `json:"message"`
	Field         string `json:"field,omitempty"`
	Expected      string `json:"expected,omitempty"`
	Actual        string `json:"actual,omitempty"`
	TileIndex     int    `json:"tile_index,omitempty"`
	TileX         int    `json:"tile_x,omitempty"`
	TileY         int    `json:"tile_y,omitempty"`
	BreakX        int    `json:"break_x,omitempty"`
	BreakY        int    `json:"break_y,omitempty"`
	LineIndex     int    `json:"line_index,omitempty"`
	Unit          string `json:"unit,omitempty"`
	DifferenceKey string `json:"difference_field,omitempty"`
	Cause         string `json:"cause,omitempty"`
}

func (e *AppError) Error() string {
	if e == nil {
		return ""
	}
	msg := fmt.Sprintf("%s: %s", e.Code, e.Message)
	if e.Field != "" {
		msg += fmt.Sprintf(" [field=%s", e.Field)
		if e.Expected != "" {
			msg += fmt.Sprintf(" expected=%s", e.Expected)
		}
		if e.Actual != "" {
			msg += fmt.Sprintf(" actual=%s", e.Actual)
		}
		msg += "]"
	}
	if e.Unit != "" {
		msg += fmt.Sprintf(" [unit=%s", e.Unit)
		if e.TileIndex >= 0 {
			msg += fmt.Sprintf(" tile=%d", e.TileIndex)
		}
		if e.LineIndex >= 0 {
			msg += fmt.Sprintf(" line=%d", e.LineIndex)
		}
		msg += "]"
	}
	return msg
}

func (e *AppError) MarshalJSON() ([]byte, error) {
	type alias AppError
	return json.Marshal((*alias)(e))
}

func NewError(code, message string) *AppError {
	return &AppError{Code: code, Message: message, TileIndex: -1, LineIndex: -1}
}

func Errorf(code, format string, args ...any) *AppError {
	return NewError(code, fmt.Sprintf(format, args...))
}

func DecodeError(message string, cause error) *AppError {
	e := NewError(CodeDecode, message)
	if cause != nil {
		e.Cause = cause.Error()
	}
	return e
}

func MetaError(field string, expected, actual any) *AppError {
	return &AppError{
		Code:      CodeMeta,
		Message:   "image metadata does not match the manifest/truth declaration",
		Field:     field,
		Expected:  fmt.Sprint(expected),
		Actual:    fmt.Sprint(actual),
		TileIndex: -1,
		LineIndex: -1,
	}
}

func ParaError(field string, expected, actual any) *AppError {
	return &AppError{
		Code:      CodePara,
		Message:   "parameter is outside the supported range",
		Field:     field,
		Expected:  fmt.Sprint(expected),
		Actual:    fmt.Sprint(actual),
		TileIndex: -1,
		LineIndex: -1,
	}
}

func StateError(unit string, tileIndex, lineIndex int, from, to State) *AppError {
	return &AppError{
		Code:      CodeState,
		Message:   fmt.Sprintf("illegal state transition %s -> %s", from, to),
		Unit:      unit,
		TileIndex: tileIndex,
		LineIndex: lineIndex,
	}
}

func AsAppError(err error) *AppError {
	if err == nil {
		return nil
	}
	if appErr, ok := err.(*AppError); ok {
		return appErr
	}
	return &AppError{Code: CodeStore, Message: err.Error(), TileIndex: -1, LineIndex: -1}
}
