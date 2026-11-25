package lightshow

import (
	"errors"
	"fmt"
)

const (
	CodeVersion  = "E_VERSION"
	CodeLossy    = "E_LOSSY"
	CodeOrder    = "E_ORDER"
	CodeGroup    = "E_GROUP"
	CodeState    = "E_STATE"
	CodeDecode   = "E_DECODE"
	CodeCanceled = "E_CANCELED"
)

const AlgorithmVersion = "stage-engine/1.0.0"

// EngineError is the stable error envelope used by every public entry point.
// Path is a JSON-like field path so callers can point at the exact input.
type EngineError struct {
	Code    string
	Path    string
	Message string
	Losses  []Loss
}

func (e *EngineError) Error() string {
	if e == nil {
		return ""
	}
	if e.Path != "" {
		return fmt.Sprintf("%s at %s: %s", e.Code, e.Path, e.Message)
	}
	return fmt.Sprintf("%s: %s", e.Code, e.Message)
}

func engineError(code, path, format string, args ...any) *EngineError {
	return &EngineError{Code: code, Path: path, Message: fmt.Sprintf(format, args...)}
}

// CodeOf returns the stable error code, or an empty string for nil/unknown errors.
func CodeOf(err error) string {
	var e *EngineError
	if errors.As(err, &e) {
		return e.Code
	}
	return ""
}

// IsCode reports whether err is an EngineError with code.
func IsCode(err error, code string) bool {
	return CodeOf(err) == code
}

// AsEngineError exposes the structured error without requiring a type assertion.
func AsEngineError(err error) (*EngineError, bool) {
	var e *EngineError
	if errors.As(err, &e) {
		return e, true
	}
	return nil, false
}

// Loss records an explicit downgrade decision. It is returned even when the
// caller chooses the non-strict Export API.
type Loss struct {
	Path        string `json:"path"`
	Value       string `json:"value"`
	Reason      string `json:"reason"`
	Disposition string `json:"disposition"`
}

func lossyError(losses []Loss) *EngineError {
	return &EngineError{
		Code:    CodeLossy,
		Message: fmt.Sprintf("%d value(s) cannot be represented losslessly", len(losses)),
		Losses:  append([]Loss(nil), losses...),
	}
}
