package model

import "fmt"

const (
	CodeSchema     = "E_SCHEMA"
	CodeParameters = "E_PARA"
	CodeState      = "E_STATE"
	CodeIO         = "E_IO"
	CodeVersion    = "E_VERSION"
	CodeCheckpoint = "E_CHECKPOINT"
	CodeInternal   = "E_INTERNAL"
)

// AppError is the single error shape exposed by the workbench. The location
// fields are deliberately explicit because the diagnostic UI must identify the
// exact window, chunk and field that failed validation.
type AppError struct {
	Code      string `json:"code"`
	Window    int    `json:"window"`
	Chunk     int    `json:"chunk"`
	Field     string `json:"field,omitempty"`
	Message   string `json:"message"`
	Detail    string `json:"detail,omitempty"`
	Invalid   []int  `json:"invalid_windows,omitempty"`
	Retryable bool   `json:"retryable,omitempty"`
}

func (e *AppError) Error() string {
	if e == nil {
		return ""
	}
	loc := ""
	if e.Window > 0 || e.Chunk > 0 || e.Field != "" {
		loc = fmt.Sprintf(" [window=%d chunk=%d field=%s]", e.Window, e.Chunk, e.Field)
	}
	if e.Detail != "" {
		return fmt.Sprintf("%s: %s%s (%s)", e.Code, e.Message, loc, e.Detail)
	}
	return fmt.Sprintf("%s: %s%s", e.Code, e.Message, loc)
}

func SchemaError(chunk int, field, message string) *AppError {
	return &AppError{Code: CodeSchema, Chunk: chunk, Field: field, Message: message}
}

func ParamError(field, message string) *AppError {
	return &AppError{Code: CodeParameters, Field: field, Message: message}
}

func StateError(window int, message string) *AppError {
	return &AppError{Code: CodeState, Window: window, Message: message}
}

func VersionError(message string) *AppError {
	return &AppError{Code: CodeVersion, Message: message}
}

func CheckpointError(message string) *AppError {
	return &AppError{Code: CodeCheckpoint, Message: message}
}

func IOError(field, message string, err error) *AppError {
	detail := ""
	if err != nil {
		detail = err.Error()
	}
	return &AppError{Code: CodeIO, Field: field, Message: message, Detail: detail}
}

func InternalError(window int, message string, err error) *AppError {
	detail := ""
	if err != nil {
		detail = err.Error()
	}
	return &AppError{Code: CodeInternal, Window: window, Message: message, Detail: detail, Retryable: true}
}
