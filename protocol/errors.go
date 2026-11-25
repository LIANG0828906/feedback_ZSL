package protocol

import (
	"errors"
	"fmt"
)

type Code string

const (
	CodeDecode   Code = "E_DECODE"
	CodeSchema   Code = "E_SCHEMA"
	CodeRef      Code = "E_REF"
	CodeState    Code = "E_STATE"
	CodePara     Code = "E_PARA"
	CodeConflict Code = "E_CONFLICT"
	CodeSeg      Code = "E_SEG"
	CodeDiff     Code = "E_DIFF"
	CodeOrder    Code = "E_ORDER"
	CodeCap      Code = "E_CAP"
	CodeStore    Code = "E_STORE"
	CodeResource Code = "E_RESOURCE"
)

type KernelError struct {
	Code      Code   `json:"code"`
	Source    string `json:"source,omitempty"`
	Path      string `json:"path,omitempty"`
	Message   string `json:"message"`
	Time      *int64 `json:"time,omitempty"`
	Window    int    `json:"window,omitempty"`
	DisplayID string `json:"display_id,omitempty"`
	Field     string `json:"field,omitempty"`
	State     string `json:"state,omitempty"`
	Cause     string `json:"cause,omitempty"`
}

func (e *KernelError) Error() string {
	if e == nil {
		return ""
	}
	prefix := string(e.Code)
	if e.Source != "" {
		prefix += " " + e.Source
	}
	if e.Path != "" {
		prefix += " " + e.Path
	}
	details := ""
	if e.Window > 0 {
		details += fmt.Sprintf(" window=%d", e.Window)
	}
	if e.DisplayID != "" {
		details += fmt.Sprintf(" display=%s", e.DisplayID)
	}
	if e.Field != "" {
		details += fmt.Sprintf(" field=%s", e.Field)
	}
	if e.Time != nil {
		details += fmt.Sprintf(" time=%d", *e.Time)
	}
	if e.Message != "" {
		details += ": " + e.Message
	}
	return prefix + details
}

func Errorf(code Code, source, path, format string, args ...any) *KernelError {
	return &KernelError{
		Code:    code,
		Source:  source,
		Path:    path,
		Message: fmt.Sprintf(format, args...),
	}
}

func WithTime(err *KernelError, time int64) *KernelError {
	if err == nil {
		return nil
	}
	err.Time = &time
	return err
}

func WithWindow(err *KernelError, window int) *KernelError {
	if err == nil {
		return nil
	}
	err.Window = window
	return err
}

func WithDisplay(err *KernelError, displayID, field string) *KernelError {
	if err == nil {
		return nil
	}
	err.DisplayID = displayID
	err.Field = field
	return err
}

func AsKernelError(err error) *KernelError {
	if err == nil {
		return nil
	}
	var kernelErr *KernelError
	if errors.As(err, &kernelErr) {
		return kernelErr
	}
	return &KernelError{
		Code:    CodeSeg,
		Message: err.Error(),
		Cause:   err.Error(),
	}
}

func IsCode(err error, code Code) bool {
	kernelErr := AsKernelError(err)
	return kernelErr != nil && kernelErr.Code == code
}
