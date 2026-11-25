package core

import (
	"encoding/json"
	"errors"
	"fmt"
	"strings"
)

type Code string

const (
	CodeSchema   Code = "E_SCHEMA"
	CodeRange    Code = "E_RANGE"
	CodePara     Code = "E_PARA"
	CodeStage    Code = "E_STAGE"
	CodeBlock    Code = "E_BLOCK"
	CodeState    Code = "E_STATE"
	CodeDiff     Code = "E_DIFF"
	CodeCanceled Code = "E_CANCELED"
)

type BlockRef struct {
	Index int `json:"index"`
	X     int `json:"x"`
	Y     int `json:"y"`
	W     int `json:"w"`
	H     int `json:"h"`
}

type Error struct {
	Code        Code           `json:"code"`
	Message     string         `json:"message"`
	Path        string         `json:"path,omitempty"`
	Step        int            `json:"step,omitempty"`
	Field       string         `json:"field,omitempty"`
	Block       *BlockRef      `json:"block,omitempty"`
	StateBefore string         `json:"state_before,omitempty"`
	StateAfter  string         `json:"state_after,omitempty"`
	Details     map[string]any `json:"details,omitempty"`
	Cause       error          `json:"-"`
}

func (e *Error) Error() string {
	if e == nil {
		return "<nil>"
	}
	var parts []string
	parts = append(parts, string(e.Code))
	if e.Step > 0 {
		parts = append(parts, fmt.Sprintf("step=%d", e.Step))
	}
	if e.Path != "" {
		parts = append(parts, "path="+e.Path)
	}
	if e.Field != "" {
		parts = append(parts, "field="+e.Field)
	}
	if e.Block != nil {
		parts = append(parts, fmt.Sprintf("block=%d@%d,%d", e.Block.Index, e.Block.X, e.Block.Y))
	}
	if e.Message != "" {
		parts = append(parts, e.Message)
	}
	if e.Cause != nil {
		parts = append(parts, "cause="+e.Cause.Error())
	}
	return strings.Join(parts, ": ")
}

func (e *Error) Unwrap() error { return e.Cause }

func (e *Error) JSON() string {
	data, _ := json.MarshalIndent(e, "", "  ")
	return string(data)
}

func NewError(code Code, message string) *Error {
	return &Error{Code: code, Message: message}
}

func Schema(path, format string, args ...any) *Error {
	return &Error{Code: CodeSchema, Path: path, Message: fmt.Sprintf(format, args...)}
}

func Range(path, format string, args ...any) *Error {
	return &Error{Code: CodeRange, Path: path, Message: fmt.Sprintf(format, args...)}
}

func Para(step int, field, format string, args ...any) *Error {
	return &Error{Code: CodePara, Step: step, Field: field, Message: fmt.Sprintf(format, args...)}
}

func Stage(step int, block BlockRef, format string, args ...any) *Error {
	return &Error{
		Code:    CodeStage,
		Step:    step,
		Block:   &block,
		Message: fmt.Sprintf(format, args...),
	}
}

func BlockError(format string, args ...any) *Error {
	return &Error{Code: CodeBlock, Message: fmt.Sprintf(format, args...)}
}

func StateError(before, after string, step int, block *BlockRef, message string) *Error {
	return &Error{
		Code:        CodeState,
		Step:        step,
		Block:       block,
		StateBefore: before,
		StateAfter:  after,
		Message:     message,
	}
}

func DiffError(step int, block *BlockRef, field, message string) *Error {
	if message == "" {
		message = "artifacts differ"
	}
	return &Error{Code: CodeDiff, Step: step, Block: block, Field: field, Message: message}
}

func CanceledError(message string) *Error {
	return &Error{Code: CodeCanceled, Message: message, Cause: errors.New(message)}
}

func As(err error) (*Error, bool) {
	var target *Error
	if errors.As(err, &target) {
		return target, true
	}
	return nil, false
}

func CodeOf(err error) Code {
	var target *Error
	if errors.As(err, &target) {
		return target.Code
	}
	return ""
}
