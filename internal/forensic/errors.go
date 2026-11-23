package forensic

import (
	"errors"
	"fmt"
)

type Code string

const (
	CodeParam    Code = "E_PARA"
	CodeDecode   Code = "E_DECODE"
	CodeMetadata Code = "E_META"
	CodeHash     Code = "E_HASH"
	CodeDiverge  Code = "E_DIVERGE"
)

// Error is the stable machine-readable error returned by the forensic engine.
// Field, ImageID, Block and Stage are populated when they are known.
type Error struct {
	Code    Code   `json:"code"`
	Field   string `json:"field,omitempty"`
	ImageID string `json:"imageId,omitempty"`
	Block   int    `json:"block,omitempty"`
	Stage   string `json:"stage,omitempty"`
	X       int    `json:"x,omitempty"`
	Y       int    `json:"y,omitempty"`
	Message string `json:"message"`
	Err     error  `json:"-"`
}

func (e *Error) Error() string {
	if e == nil {
		return ""
	}
	msg := string(e.Code)
	if e.ImageID != "" {
		msg += " image=" + e.ImageID
	}
	if e.Field != "" {
		msg += " field=" + e.Field
	}
	if e.Block >= 0 && e.Stage != "" {
		msg += fmt.Sprintf(" block=%d stage=%s", e.Block, e.Stage)
	} else if e.Block >= 0 {
		msg += fmt.Sprintf(" block=%d", e.Block)
	}
	if e.Message != "" {
		msg += ": " + e.Message
	}
	if e.Err != nil {
		msg += ": " + e.Err.Error()
	}
	return msg
}

func (e *Error) Unwrap() error { return e.Err }

func NewError(code Code, message string) *Error {
	return &Error{Code: code, Block: -1, Message: message}
}

func ParamError(field, message string) *Error {
	return &Error{Code: CodeParam, Field: field, Block: -1, Message: message}
}

func DecodeError(message string, err error) *Error {
	return &Error{Code: CodeDecode, Block: -1, Message: message, Err: err}
}

func MetaError(imageID, field, message string) *Error {
	return &Error{Code: CodeMetadata, ImageID: imageID, Field: field, Block: -1, Message: message}
}

func AsError(err error) (*Error, bool) {
	var target *Error
	if errors.As(err, &target) {
		return target, true
	}
	return nil, false
}
