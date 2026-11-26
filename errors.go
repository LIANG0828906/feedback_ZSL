package lightprobe

import "fmt"

const (
	ECancel = "E_CANCEL"
	EDecode = "E_DECODE"
	EDiff   = "E_DIFF"
	EState  = "E_STATE"
	ESchema = "E_SCHEMA"
	EVal    = "E_VAL"
)

// Error is the structured domain error used by all acceptance-facing failures.
type Error struct {
	Code     string `json:"code"`
	ObjectID string `json:"object_id"`
	Field    string `json:"field"`
	Message  string `json:"message"`
}

func (e *Error) Error() string {
	return fmt.Sprintf("%s object=%s field=%s: %s", e.Code, e.ObjectID, e.Field, e.Message)
}

func domainError(code, objectID, field, format string, args ...any) *Error {
	return &Error{
		Code:     code,
		ObjectID: objectID,
		Field:    field,
		Message:  fmt.Sprintf(format, args...),
	}
}

func AsDomainError(err error) (*Error, bool) {
	if err == nil {
		return nil, false
	}
	if de, ok := err.(*Error); ok {
		return de, true
	}
	return nil, false
}
