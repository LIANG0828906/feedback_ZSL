package colortrace

import (
	"errors"
	"fmt"
)

// Code is the stable machine-readable error category used by the pipeline.
type Code string

const (
	ESchema Code = "E_SCHEMA"
	EPara   Code = "E_PARA"
	EAmbig  Code = "E_AMBIG"
	EBound  Code = "E_BOUND"
	EState  Code = "E_STATE"
	EDecode Code = "E_DECODE"
	EOrder  Code = "E_ORDER"
)

// Share is an observed operator share used by E_AMBIG diagnostics.
type Share struct {
	Operator string  `json:"operator"`
	Share    float64 `json:"share"`
	AbsNum   int64   `json:"abs_num,omitempty"`
	AbsDen   int64   `json:"abs_den,omitempty"`
}

// Error carries a stable code and enough location information for diagnostics.
type Error struct {
	Code       Code    `json:"code"`
	Path       string  `json:"path,omitempty"`
	Stage      int     `json:"stage,omitempty"`
	HasStage   bool    `json:"has_stage,omitempty"`
	Chunk      string  `json:"chunk,omitempty"`
	Coord      *Coord  `json:"coord,omitempty"`
	Message    string  `json:"message"`
	LegalRange [2]int  `json:"legal_range,omitempty"`
	Shares     []Share `json:"shares,omitempty"`
}

func (e *Error) Error() string {
	where := ""
	if e.Path != "" {
		where = " path=" + e.Path
	}
	if e.HasStage {
		where += fmt.Sprintf(" stage=%d", e.Stage)
	}
	if e.Chunk != "" {
		where += " chunk=" + e.Chunk
	}
	if e.Coord != nil {
		where += fmt.Sprintf(" coord=(%d,%d)", e.Coord.X, e.Coord.Y)
	}
	return fmt.Sprintf("%s%s: %s", e.Code, where, e.Message)
}

func (e *Error) Is(target error) bool {
	var t *Error
	if errors.As(target, &t) {
		return t.Code == e.Code
	}
	return false
}

func schemaError(path, format string, args ...any) *Error {
	return &Error{Code: ESchema, Path: path, Message: fmt.Sprintf(format, args...)}
}

func paraError(stage int, minBits, maxBits int, format string, args ...any) *Error {
	return &Error{
		Code:       EPara,
		Stage:      stage,
		HasStage:   stage >= 0,
		LegalRange: [2]int{minBits, maxBits},
		Message:    fmt.Sprintf(format, args...),
	}
}

func stateError(stage int, chunk string, from, to UnitState) *Error {
	return &Error{
		Code:     EState,
		Stage:    stage,
		HasStage: stage >= 0,
		Chunk:    chunk,
		Message:  fmt.Sprintf("illegal unit transition %s -> %s", from, to),
	}
}

// AsError returns *Error when err has one wrapped.
func AsError(err error) (*Error, bool) {
	var ce *Error
	if errors.As(err, &ce) {
		return ce, true
	}
	return nil, false
}
