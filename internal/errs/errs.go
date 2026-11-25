package errs

import (
	"errors"
	"fmt"
)

const (
	ESchema = "E_SCHEMA"
	ELossy  = "E_LOSSY"
	EOvf    = "E_OVF"
	EEnv    = "E_ENV"
	EFit    = "E_FIT"
	EState  = "E_STATE"
)

type Detail struct {
	Code          string         `json:"code"`
	Message       string         `json:"message"`
	FieldPath     string         `json:"field_path,omitempty"`
	SourceVersion string         `json:"source_version,omitempty"`
	FrameIndex    *int           `json:"frame_index,omitempty"`
	PhonemeIndex  *int           `json:"phoneme_index,omitempty"`
	Operands      []int64        `json:"operands,omitempty"`
	Context       map[string]any `json:"context,omitempty"`
}

type Error struct {
	Detail Detail
	Err    error
}

func (e *Error) Error() string {
	d := e.Detail
	msg := fmt.Sprintf("%s: %s", d.Code, d.Message)
	if d.FieldPath != "" {
		msg += " field=" + d.FieldPath
	}
	if d.SourceVersion != "" {
		msg += " schema=" + d.SourceVersion
	}
	if d.FrameIndex != nil {
		msg += fmt.Sprintf(" frame=%d", *d.FrameIndex)
	}
	if d.PhonemeIndex != nil {
		msg += fmt.Sprintf(" phoneme=%d", *d.PhonemeIndex)
	}
	if len(d.Operands) == 2 {
		msg += fmt.Sprintf(" operands=[%d,%d]", d.Operands[0], d.Operands[1])
	}
	if e.Err != nil {
		msg += ": " + e.Err.Error()
	}
	return msg
}

func (e *Error) Unwrap() error { return e.Err }

func New(d Detail) *Error {
	if d.Code == "" {
		d.Code = EState
	}
	return &Error{Detail: d}
}

func Wrap(code, msg string, err error) *Error {
	return &Error{Detail: Detail{Code: code, Message: msg}, Err: err}
}

func Schema(sourceVersion, fieldPath, msg string, args ...any) *Error {
	return New(Detail{
		Code:          ESchema,
		Message:       fmt.Sprintf(msg, args...),
		FieldPath:     fieldPath,
		SourceVersion: sourceVersion,
	})
}

func Ovf(fieldPath string, a, b int64) *Error {
	return New(Detail{
		Code:      EOvf,
		Message:   "integer conversion overflow",
		FieldPath: fieldPath,
		Operands:  []int64{a, b},
	})
}

func Env(frame, phoneme int, msg string) *Error {
	f, p := frame, phoneme
	return New(Detail{
		Code:         EEnv,
		Message:      msg,
		FrameIndex:   &f,
		PhonemeIndex: &p,
	})
}

func Fit(phoneme int, msg string, context map[string]any) *Error {
	p := phoneme
	return New(Detail{
		Code:         EFit,
		Message:      msg,
		PhonemeIndex: &p,
		Context:      context,
	})
}

func As(err error) (*Error, bool) {
	var target *Error
	if errors.As(err, &target) {
		return target, true
	}
	return nil, false
}
