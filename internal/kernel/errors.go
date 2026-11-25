package kernel

import "fmt"

type ErrorCode string

const (
	CodeSchema ErrorCode = "E_SCHEMA"
	CodeDecode ErrorCode = "E_DECODE"
	CodeOrder  ErrorCode = "E_ORDER"
	CodePara   ErrorCode = "E_PARA"
	CodeOvf    ErrorCode = "E_OVF"
	CodeLossy  ErrorCode = "E_LOSSY"
	CodeBlock  ErrorCode = "E_BLOCK"
)

type Issue struct {
	Code          ErrorCode `json:"code"`
	Message       string    `json:"message"`
	Field         string    `json:"field,omitempty"`
	SourceVersion int       `json:"source_version,omitempty"`
	ID            string    `json:"id,omitempty"`
	OldValue      string    `json:"old_value,omitempty"`
	DefaultValue  string    `json:"default_value,omitempty"`
	Disposition   string    `json:"disposition,omitempty"`
	Operands      []string  `json:"operands,omitempty"`
}

type Error struct {
	Issue
}

func (e *Error) Error() string {
	if e.Field != "" {
		return fmt.Sprintf("%s field=%s id=%q v%d: %s", e.Code, e.Field, e.ID, e.SourceVersion, e.Message)
	}
	return fmt.Sprintf("%s id=%q v%d: %s", e.Code, e.ID, e.SourceVersion, e.Message)
}

func newIssue(code ErrorCode, field string, version int, id string, format string, args ...any) Issue {
	return Issue{
		Code:          code,
		Field:         field,
		SourceVersion: version,
		ID:            id,
		Message:       fmt.Sprintf(format, args...),
	}
}

func asError(issue Issue) error {
	return &Error{Issue: issue}
}

func schemaError(field string, version int, format string, args ...any) error {
	return asError(newIssue(CodeSchema, field, version, "", format, args...))
}

func parameterError(field, id string, version int, format string, args ...any) error {
	return asError(newIssue(CodePara, field, version, id, format, args...))
}

func overflowError(field, id string, version int, a, b any) error {
	issue := newIssue(CodeOvf, field, version, id, "integer overflow for operands %v and %v", a, b)
	issue.Operands = []string{fmt.Sprint(a), fmt.Sprint(b)}
	return asError(issue)
}

func orderError(field, id string, version int, format string, args ...any) error {
	return asError(newIssue(CodeOrder, field, version, id, format, args...))
}

func NewOrderError(field, id, format string, args ...any) error {
	return orderError(field, id, 0, format, args...)
}

func blockError(tileID string, format string, args ...any) error {
	return asError(Issue{
		Code:    CodeBlock,
		Field:   "$.tile",
		ID:      tileID,
		Message: fmt.Sprintf(format, args...),
	})
}
