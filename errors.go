package vpcompose

import (
	"errors"
	"fmt"
	"strings"
)

type ErrorCode string

const (
	CodeSchema   ErrorCode = "E_SCHEMA"
	CodeCycle    ErrorCode = "E_CYCLE"
	CodeLossy    ErrorCode = "E_LOSSY"
	CodeOverflow ErrorCode = "E_OVF"
	CodePixel    ErrorCode = "E_PIX"
	CodeState    ErrorCode = "E_STATE"
	CodeDiff     ErrorCode = "E_DIFF"
	CodeDecode   ErrorCode = "E_DECODE"
	CodeBudget   ErrorCode = "E_BUDGET"
)

// Loss describes a value that the target schema cannot represent exactly.
type Loss struct {
	Path        string `json:"path"`
	Kind        string `json:"kind"`
	Disposition string `json:"disposition"`
	Detail      string `json:"detail,omitempty"`
}

// EngineError is the public, machine-readable error used by every layer.
type EngineError struct {
	Code     ErrorCode `json:"code"`
	Path     string    `json:"path,omitempty"`
	Message  string    `json:"message,omitempty"`
	Operands []int64   `json:"operands,omitempty"`
	LayerIDs []string  `json:"layerIds,omitempty"`
	Losses   []Loss    `json:"losses,omitempty"`
	Window   int       `json:"window,omitempty"`
	Field    string    `json:"field,omitempty"`
	Expected any       `json:"expected,omitempty"`
	Actual   any       `json:"actual,omitempty"`
	Cause    error     `json:"-"`
}

func (e *EngineError) Error() string {
	var b strings.Builder
	b.WriteString(string(e.Code))
	if e.Path != "" {
		b.WriteString(" path=")
		b.WriteString(e.Path)
	}
	if e.Message != "" {
		b.WriteString(": ")
		b.WriteString(e.Message)
	}
	if len(e.Operands) > 0 {
		fmt.Fprintf(&b, " operands=%v", e.Operands)
	}
	if len(e.LayerIDs) > 0 {
		fmt.Fprintf(&b, " layers=%v", e.LayerIDs)
	}
	if e.Window != 0 || e.Field != "" {
		fmt.Fprintf(&b, " window=%d field=%s", e.Window, e.Field)
	}
	if len(e.Losses) > 0 {
		fmt.Fprintf(&b, " losses=%d", len(e.Losses))
	}
	if e.Cause != nil {
		fmt.Fprintf(&b, " cause=%v", e.Cause)
	}
	return b.String()
}

func (e *EngineError) Unwrap() error { return e.Cause }

func IsCode(err error, code ErrorCode) bool {
	var target *EngineError
	return errors.As(err, &target) && target.Code == code
}

func newError(code ErrorCode, path, message string) *EngineError {
	return &EngineError{Code: code, Path: path, Message: message}
}

func schemaError(path, message string) *EngineError {
	return newError(CodeSchema, path, message)
}

func decodeError(path string, cause error) *EngineError {
	return &EngineError{Code: CodeDecode, Path: path, Message: "invalid encoded input", Cause: cause}
}

func overflowError(path string, a, b int64) *EngineError {
	return &EngineError{
		Code:     CodeOverflow,
		Path:     path,
		Message:  "integer conversion overflow",
		Operands: []int64{a, b},
	}
}
