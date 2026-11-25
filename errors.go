package maskqc

import "fmt"

const (
	CodeSchema     = "E_SCHEMA"
	CodeOverflow   = "E_OVF"
	CodeParameter  = "E_PARA"
	CodeTopology   = "E_TOP"
	CodeCheckpoint = "E_CHK"
	CodeBudget     = "E_BUDGET"
	CodeInternal   = "E_INTERNAL"
)

// QCError is the structured error returned by all loaders and verifiers.
type QCError struct {
	Code       string         `json:"code"`
	Field      string         `json:"field,omitempty"`
	Path       string         `json:"path,omitempty"`
	Message    string         `json:"message"`
	Operands   []int64        `json:"operands,omitempty"`
	LegalRange []int          `json:"legalRange,omitempty"`
	Details    map[string]any `json:"details,omitempty"`
}

func (e *QCError) Error() string {
	location := e.Path
	if location == "" {
		location = e.Field
	}
	if location != "" {
		return fmt.Sprintf("%s at %s: %s", e.Code, location, e.Message)
	}
	return fmt.Sprintf("%s: %s", e.Code, e.Message)
}

func schemaError(path, format string, args ...any) *QCError {
	return &QCError{Code: CodeSchema, Path: path, Field: path, Message: fmt.Sprintf(format, args...)}
}

func parameterError(field string, low, high int, format string, args ...any) *QCError {
	return &QCError{
		Code:       CodeParameter,
		Field:      field,
		Path:       "$." + field,
		Message:    fmt.Sprintf(format, args...),
		LegalRange: []int{low, high},
	}
}

func overflowError(field string, a, b int64) *QCError {
	return &QCError{
		Code:     CodeOverflow,
		Field:    field,
		Path:     "$." + field,
		Message:  "integer conversion overflow",
		Operands: []int64{a, b},
	}
}
