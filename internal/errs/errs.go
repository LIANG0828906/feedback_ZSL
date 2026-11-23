package errs

import (
	"encoding/json"
	"fmt"
	"net/http"
)

const (
	EDecode     = "E_DECODE"
	EMeta       = "E_META"
	EPara       = "E_PARA"
	EMatch      = "E_MATCH"
	EHomo       = "E_HOMO"
	EDiverge    = "E_DIVERGE"
	EVersion    = "E_VER"
	ECheckpoint = "E_CHECKPOINT"
	EInternal   = "E_INTERNAL"
	EConflict   = "E_CONFLICT"
)

// Error is the stable, machine-readable error envelope used by the CLI and API.
type Error struct {
	Code      string         `json:"code"`
	Message   string         `json:"message"`
	Field     string         `json:"field,omitempty"`
	Actual    any            `json:"actual,omitempty"`
	Threshold any            `json:"threshold,omitempty"`
	Details   map[string]any `json:"details,omitempty"`
	Cause     error          `json:"-"`
}

func (e *Error) Error() string {
	if e == nil {
		return ""
	}
	if e.Field != "" {
		return fmt.Sprintf("%s: %s (%s)", e.Code, e.Message, e.Field)
	}
	return fmt.Sprintf("%s: %s", e.Code, e.Message)
}

func (e *Error) Unwrap() error { return e.Cause }

func New(code, message string) *Error {
	return &Error{Code: code, Message: message}
}

func Field(code, message, field string) *Error {
	return &Error{Code: code, Message: message, Field: field}
}

func Wrap(code, message string, cause error) *Error {
	return &Error{Code: code, Message: message, Cause: cause}
}

func Detail(code, message string, details map[string]any) *Error {
	return &Error{Code: code, Message: message, Details: details}
}

// As normalizes arbitrary errors to the API envelope.
func As(err error) *Error {
	if err == nil {
		return nil
	}
	if e, ok := err.(*Error); ok {
		return e
	}
	return &Error{Code: EInternal, Message: err.Error(), Cause: err}
}

func HTTPStatus(code string) int {
	switch code {
	case EDecode:
		return http.StatusUnprocessableEntity
	case EMeta, EPara, EMatch, EHomo, EDiverge, EVersion, EConflict:
		return http.StatusUnprocessableEntity
	case ECheckpoint:
		return http.StatusConflict
	default:
		return http.StatusInternalServerError
	}
}

func WriteJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}
