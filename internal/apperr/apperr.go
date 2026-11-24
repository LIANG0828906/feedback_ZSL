package apperr

import (
	"encoding/json"
	"errors"
	"fmt"
)

const (
	EDecode     = "E_DECODE"
	EPlan       = "E_PLAN"
	EPara       = "E_PARA"
	EState      = "E_STATE"
	EVersion    = "E_VERSION"
	EDiff       = "E_DIFF"
	EIO         = "E_IO"
	EConcurrent = "E_CONCURRENCY"
	ECheckpoint = "E_CHECKPOINT"
)

// Error is the stable error envelope returned by every public operation.
type Error struct {
	Code       string   `json:"code"`
	Message    string   `json:"message"`
	EntryIndex int      `json:"entry_index,omitempty"`
	Field      string   `json:"field,omitempty"`
	Segment    int      `json:"segment,omitempty"`
	Version    int      `json:"version,omitempty"`
	Other      int      `json:"other_version,omitempty"`
	Coordinate *Point   `json:"coordinate,omitempty"`
	Expected   []uint16 `json:"expected,omitempty"`
	Actual     []uint16 `json:"actual,omitempty"`
	Path       string   `json:"path,omitempty"`
	Details    string   `json:"details,omitempty"`
}

type Point struct {
	X int `json:"x"`
	Y int `json:"y"`
}

func (e *Error) Error() string {
	if e == nil {
		return ""
	}
	b, _ := json.Marshal(e)
	return string(b)
}

func New(code, format string, args ...any) *Error {
	return &Error{Code: code, Message: fmt.Sprintf(format, args...)}
}

func At(code string, entry int, field, format string, args ...any) *Error {
	return &Error{Code: code, EntryIndex: entry, Field: field, Message: fmt.Sprintf(format, args...)}
}

func As(err error) *Error {
	if err == nil {
		return nil
	}
	var ae *Error
	if errors.As(err, &ae) {
		return ae
	}
	return &Error{Code: EIO, Message: err.Error()}
}

func IsCode(err error, code string) bool {
	ae := As(err)
	return ae != nil && ae.Code == code
}
