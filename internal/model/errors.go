package model

import (
	"fmt"
	"strings"
)

type ErrorCode string

const (
	ErrSchema ErrorCode = "E_SCHEMA"
	ErrCycle  ErrorCode = "E_CYCLE"
	ErrFrame  ErrorCode = "E_FRAME"
	ErrMap    ErrorCode = "E_MAP"
	ErrParam  ErrorCode = "E_PARA"
	ErrInfeas ErrorCode = "E_INFEAS"
	ErrMark   ErrorCode = "E_MARK"
	ErrScale  ErrorCode = "E_SCALE"
	ErrState  ErrorCode = "E_STATE"
)

// AppError carries stable machine-readable diagnostics. Frame is -1 when the
// error is not tied to a frame.
type AppError struct {
	Code    ErrorCode `json:"code"`
	Object  string    `json:"object,omitempty"`
	Frame   int       `json:"frame"`
	Joint   string    `json:"joint,omitempty"`
	Field   string    `json:"field,omitempty"`
	Message string    `json:"message"`
	Details []string  `json:"details,omitempty"`
}

func (e *AppError) Error() string {
	parts := []string{string(e.Code)}
	if e.Object != "" {
		parts = append(parts, "object="+e.Object)
	}
	if e.Frame >= 0 {
		parts = append(parts, fmt.Sprintf("frame=%d", e.Frame))
	}
	if e.Joint != "" {
		parts = append(parts, "joint="+e.Joint)
	}
	if e.Field != "" {
		parts = append(parts, "field="+e.Field)
	}
	msg := strings.Join(parts, " ")
	if e.Message != "" {
		msg += ": " + e.Message
	}
	if len(e.Details) > 0 {
		msg += " [" + strings.Join(e.Details, ", ") + "]"
	}
	return msg
}

func NewError(code ErrorCode, object, field, message string) *AppError {
	return &AppError{Code: code, Object: object, Frame: -1, Field: field, Message: message}
}

func FrameError(code ErrorCode, object string, frame int, field, message string) *AppError {
	return &AppError{Code: code, Object: object, Frame: frame, Field: field, Message: message}
}
