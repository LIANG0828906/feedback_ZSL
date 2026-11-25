package errs

import (
	"errors"
	"fmt"
	"strings"
)

const (
	EDecode   = "E_DECODE"
	EPara     = "E_PARA"
	EIter     = "E_ITER"
	EEdge     = "E_EDGE"
	EState    = "E_STATE"
	EDiverge  = "E_DIVERGE"
	ECheckpt  = "E_CHECKPOINT"
	EInternal = "E_INTERNAL"
)

type Violation struct {
	Field      string    `json:"field,omitempty"`
	Message    string    `json:"message,omitempty"`
	LegalRange [2]any    `json:"legal_range,omitempty"`
	Tile       [2]int    `json:"tile,omitempty"`
	Stage      string    `json:"stage,omitempty"`
	Residual   float64   `json:"residual,omitempty"`
	EdgeID     int       `json:"edge_id,omitempty"`
	LeftCoord  []float64 `json:"left_coord,omitempty"`
	RightCoord []float64 `json:"right_coord,omitempty"`
	Measured   []float64 `json:"measured,omitempty"`
	Tolerances []float64 `json:"tolerances,omitempty"`
	FirstTile  [2]int    `json:"first_tile,omitempty"`
	DiffPixel  [2]int    `json:"diff_pixel,omitempty"`
	BadState   string    `json:"bad_state,omitempty"`
	FromState  string    `json:"from_state,omitempty"`
	ToState    string    `json:"to_state,omitempty"`
}

type Error struct {
	Code      string    `json:"code"`
	Message   string    `json:"message"`
	Violation Violation `json:"violation,omitempty"`
	Cause     error     `json:"-"`
}

func (e *Error) Error() string {
	var b strings.Builder
	fmt.Fprintf(&b, "%s: %s", e.Code, e.Message)
	if e.Violation.Field != "" {
		fmt.Fprintf(&b, " field=%s", e.Violation.Field)
	}
	if e.Violation.Tile != [2]int{} {
		fmt.Fprintf(&b, " tile=(%d,%d)", e.Violation.Tile[0], e.Violation.Tile[1])
	}
	if e.Violation.Stage != "" {
		fmt.Fprintf(&b, " stage=%s", e.Violation.Stage)
	}
	if e.Violation.Residual != 0 {
		fmt.Fprintf(&b, " residual=%.6f", e.Violation.Residual)
	}
	if e.Cause != nil {
		fmt.Fprintf(&b, ": %v", e.Cause)
	}
	return b.String()
}

func (e *Error) Unwrap() error { return e.Cause }

func New(code, message string) *Error {
	return &Error{Code: code, Message: message}
}

func Para(field string, value any, legal [2]any) *Error {
	return &Error{
		Code:    EPara,
		Message: fmt.Sprintf("parameter %s=%v is outside legal range", field, value),
		Violation: Violation{
			Field:      field,
			LegalRange: legal,
		},
	}
}

func Decode(path string, cause error) *Error {
	return &Error{Code: EDecode, Message: "image or sample payload is corrupt: " + path, Cause: cause}
}

func Iter(tile [2]int, residual float64, tolerance float64) *Error {
	return &Error{
		Code:      EIter,
		Message:   fmt.Sprintf("iteration budget exhausted with residual %.6f > tolerance %.6f", residual, tolerance),
		Violation: Violation{Tile: tile, Residual: residual, LegalRange: [2]any{0.0, tolerance}},
	}
}

func State(tile [2]int, stage, from, to string) *Error {
	return &Error{
		Code:      EState,
		Message:   fmt.Sprintf("illegal state transition %s -> %s", from, to),
		Violation: Violation{Tile: tile, Stage: stage, FromState: from, ToState: to, BadState: from},
	}
}

func As(err error) (*Error, bool) {
	var ee *Error
	if errors.As(err, &ee) {
		return ee, true
	}
	return nil, false
}
