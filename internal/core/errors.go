package core

import "fmt"

const (
	CodeDecode   = "E_DECODE"
	CodeMeta     = "E_META"
	CodeMark     = "E_MARK"
	CodePara     = "E_PARA"
	CodeDiff     = "E_DIFF"
	CodeTxn      = "E_TXN"
	CodeResource = "E_RESOURCE"
	CodeInternal = "E_INTERNAL"
)

type Error struct {
	Code         string `json:"code"`
	Message      string `json:"message"`
	Frame        *int   `json:"frame,omitempty"`
	Field        string `json:"field,omitempty"`
	Batch        *int   `json:"batch,omitempty"`
	EvidenceType string `json:"evidence_type,omitempty"`
	BlockX       *int   `json:"block_x,omitempty"`
	BlockY       *int   `json:"block_y,omitempty"`
	Other        string `json:"other,omitempty"`
}

func (e *Error) Error() string {
	s := e.Code + ": " + e.Message
	if e.Frame != nil {
		s += fmt.Sprintf(" (frame=%d", *e.Frame)
		if e.Field != "" {
			s += ", field=" + e.Field
		}
		s += ")"
	} else if e.Field != "" {
		s += " (field=" + e.Field + ")"
	}
	if e.Batch != nil {
		s += fmt.Sprintf(" (batch=%d)", *e.Batch)
	}
	return s
}

func NewError(code, format string, args ...any) *Error {
	return &Error{Code: code, Message: fmt.Sprintf(format, args...)}
}

func WithFrame(err *Error, frame int, field string) *Error {
	err.Frame = &frame
	if field != "" {
		err.Field = field
	}
	return err
}

func WithField(err *Error, field string) *Error {
	err.Field = field
	return err
}

func WithBatch(err *Error, batch int) *Error {
	err.Batch = &batch
	return err
}

func AsError(err error) (*Error, bool) {
	if err == nil {
		return nil, false
	}
	if e, ok := err.(*Error); ok {
		return e, true
	}
	return nil, false
}
