package core

import (
	"errors"
	"fmt"
)

type Code string

const (
	CodeSchema   Code = "E_SCHEMA"
	CodeState    Code = "E_STATE"
	CodeParam    Code = "E_PARA"
	CodeDiverge  Code = "E_DIVERGE"
	CodeTxn      Code = "E_TXN"
	CodeDecode   Code = "E_DECODE"
	CodeUnit     Code = "E_UNIT"
	CodeResource Code = "E_RES"
	CodeCheckpt  Code = "E_CHECKPOINT"
	CodeCancel   Code = "E_CANCEL"
	CodeIO       Code = "E_IO"
)

// Error is the stable error envelope used by the CLI, HTTP API and tests.
// Path points at a JSON field, while NodeID/IntentID identify domain objects.
type Error struct {
	Code      Code   `json:"code"`
	Path      string `json:"path,omitempty"`
	NodeID    string `json:"node_id,omitempty"`
	IntentID  *int64 `json:"intent_id,omitempty"`
	IntentIdx *int   `json:"intent_index,omitempty"`
	Field     string `json:"field,omitempty"`
	BatchID   string `json:"batch_id,omitempty"`
	Tick      *int64 `json:"tick,omitempty"`
	Left      any    `json:"left,omitempty"`
	Right     any    `json:"right,omitempty"`
	Message   string `json:"message"`
	Retryable bool   `json:"retryable,omitempty"`
	Cause     string `json:"cause,omitempty"`
}

func (e *Error) Error() string {
	if e == nil {
		return "<nil>"
	}
	s := string(e.Code)
	if e.Path != "" {
		s += " at " + e.Path
	}
	if e.NodeID != "" {
		s += " node=" + e.NodeID
	}
	if e.IntentID != nil {
		s += fmt.Sprintf(" intent=%d", *e.IntentID)
	} else if e.IntentIdx != nil {
		s += fmt.Sprintf(" intent_index=%d", *e.IntentIdx)
	}
	if e.Field != "" {
		s += " field=" + e.Field
	}
	if e.Tick != nil {
		s += fmt.Sprintf(" tick=%d", *e.Tick)
	}
	if e.Left != nil || e.Right != nil {
		s += fmt.Sprintf(" left=%v right=%v", e.Left, e.Right)
	}
	if e.Message != "" {
		s += ": " + e.Message
	}
	return s
}

func (e *Error) Unwrap() error {
	if e.Cause == "" {
		return nil
	}
	return errors.New(e.Cause)
}

func NewError(code Code, message string) *Error {
	return &Error{Code: code, Message: message}
}

func (e *Error) At(path string) *Error {
	e.Path = path
	return e
}

func (e *Error) ForNode(id string) *Error {
	e.NodeID = id
	return e
}

func (e *Error) ForIntent(id int64, idx int) *Error {
	e.IntentID = &id
	e.IntentIdx = &idx
	return e
}

func (e *Error) WithField(field string) *Error {
	e.Field = field
	return e
}

func (e *Error) WithBatch(id string) *Error {
	e.BatchID = id
	return e
}

func (e *Error) WithTick(tick int64) *Error {
	e.Tick = &tick
	return e
}

func (e *Error) WithValues(left, right any) *Error {
	e.Left = left
	e.Right = right
	return e
}

func (e *Error) Wrap(err error) *Error {
	if err != nil {
		e.Cause = err.Error()
	}
	return e
}

func AsError(err error) *Error {
	var e *Error
	if errors.As(err, &e) {
		return e
	}
	return NewError(CodeState, err.Error())
}

func CodeOf(err error) Code {
	if err == nil {
		return ""
	}
	return AsError(err).Code
}
