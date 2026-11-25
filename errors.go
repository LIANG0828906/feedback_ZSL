package iconmask

import (
	"errors"
	"fmt"
)

const (
	CodeSchema     = "E_SCHEMA"
	CodeParameter  = "E_PARA"
	CodeRadius     = "E_RADIUS"
	CodeIteration  = "E_ITER"
	CodeState      = "E_STATE"
	CodeDivergence = "E_DIVERGE"
	CodeCheckpoint = "E_CHECKPOINT"
)

// Error is the single structured error type exposed by the domain package.
// It intentionally carries the image/field context at every layer.
type Error struct {
	Code        string         `json:"code"`
	ImageID     string         `json:"imageId,omitempty"`
	Field       string         `json:"field,omitempty"`
	Message     string         `json:"message"`
	Allowed     []string       `json:"allowed,omitempty"`
	Block       *Coord         `json:"block,omitempty"`
	Stage       string         `json:"stage,omitempty"`
	SegmentID   string         `json:"segmentId,omitempty"`
	Round       *int           `json:"round,omitempty"`
	Remaining   int            `json:"remainingPixels,omitempty"`
	Details     map[string]any `json:"details,omitempty"`
	FirstDiffXY *PointInt      `json:"firstDiff,omitempty"`
}

func (e *Error) Error() string {
	if e == nil {
		return ""
	}
	where := ""
	if e.ImageID != "" {
		where += " image=" + e.ImageID
	}
	if e.Field != "" {
		where += " field=" + e.Field
	}
	return fmt.Sprintf("%s%s: %s", e.Code, where, e.Message)
}

func IsCode(err error, code string) bool {
	var domainErr *Error
	return errors.As(err, &domainErr) && domainErr.Code == code
}

func domainError(code, imageID, field, message string) *Error {
	return &Error{Code: code, ImageID: imageID, Field: field, Message: message}
}

func schemaError(imageID, field, message string) *Error {
	return domainError(CodeSchema, imageID, field, message)
}

func parameterError(imageID, field, message string, allowed ...string) *Error {
	e := domainError(CodeParameter, imageID, field, message)
	e.Allowed = allowed
	return e
}

func stateError(block Coord, stage, message string) *Error {
	return &Error{
		Code:    CodeState,
		Message: message,
		Block:   &block,
		Stage:   stage,
	}
}
