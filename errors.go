package strokelab

import (
	"errors"
	"fmt"
)

// Error codes are part of the component contract. Callers should use errors.As
// with *CodedError instead of matching error strings.
const (
	CodeVersion    = "E_VER"
	CodeLossy      = "E_LOSSY"
	CodeOverflow   = "E_OVF"
	CodeRoundTrip  = "E_RT"
	CodeOrder      = "E_ORD"
	CodeTopology   = "E_TOP"
	CodeBudget     = "E_BUDGET"
	CodeCheckpoint = "E_CKPT"
	CodeCanceled   = "E_CANCELED"
	CodeExecution  = "E_EXEC"
)

// CodedError carries a stable machine-readable code and optional diagnostic
// payload. Details are intentionally opaque so callers can attach reports,
// loss lists, or topology evidence without coupling the error layer to them.
type CodedError struct {
	Code    string
	Message string
	Details any
	Err     error
}

func (e *CodedError) Error() string {
	if e == nil {
		return ""
	}
	if e.Message == "" {
		return e.Code
	}
	return e.Code + ": " + e.Message
}

func (e *CodedError) Unwrap() error { return e.Err }

func coded(code, format string, args ...any) *CodedError {
	return &CodedError{Code: code, Message: fmt.Sprintf(format, args...)}
}

func withDetails(code, format string, details any, args ...any) *CodedError {
	return &CodedError{Code: code, Message: fmt.Sprintf(format, args...), Details: details}
}

func ErrorCode(err error) string {
	var ce *CodedError
	if errors.As(err, &ce) {
		return ce.Code
	}
	return ""
}

func isCode(err error, code string) bool {
	return ErrorCode(err) == code
}
