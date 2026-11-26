package domain

import "fmt"

type ErrorCode string

const (
	CodeSchema   ErrorCode = "E_SCHEMA"
	CodeLossy    ErrorCode = "E_LOSSY"
	CodeOvf      ErrorCode = "E_OVF"
	CodeLip      ErrorCode = "E_LIP"
	CodeDiverge  ErrorCode = "E_DIVERGE"
	CodeState    ErrorCode = "E_STATE"
	CodeOrder    ErrorCode = "E_ORDER"
	CodeDecode   ErrorCode = "E_DECODE"
	CodeVal      ErrorCode = "E_VAL"
	CodeStage    ErrorCode = "E_STAGE"
	CodeNotFound ErrorCode = "E_NOT_FOUND"
)

type Error struct {
	Code          ErrorCode      `json:"code"`
	Message       string         `json:"message"`
	Path          string         `json:"path,omitempty"`
	DialogueID    string         `json:"dialogue_id,omitempty"`
	ShardID       string         `json:"shard_id,omitempty"`
	ShardIndex    *int           `json:"shard_index,omitempty"`
	Frame         *int64         `json:"frame,omitempty"`
	PackIndex     *int           `json:"pack_index,omitempty"`
	Field         string         `json:"field,omitempty"`
	OperandA      string         `json:"operand_a,omitempty"`
	OperandB      string         `json:"operand_b,omitempty"`
	Expected      any            `json:"expected,omitempty"`
	Actual        any            `json:"actual,omitempty"`
	Detail        map[string]any `json:"detail,omitempty"`
	Disposition   string         `json:"disposition,omitempty"`
	OldValue      any            `json:"old_value,omitempty"`
	DefaultValue  any            `json:"default_value,omitempty"`
	Residual      int64          `json:"residual,omitempty"`
	TruncatedAt   string         `json:"truncated_at,omitempty"`
	SourceVersion SchemaVersion  `json:"source_version,omitempty"`
}

func (e *Error) Error() string {
	where := ""
	if e.Path != "" {
		where = " at " + e.Path
	}
	ids := ""
	if e.DialogueID != "" {
		ids += " dialogue=" + e.DialogueID
	}
	if e.ShardIndex != nil {
		ids += fmt.Sprintf(" shard=%d", *e.ShardIndex)
	}
	if e.Frame != nil {
		ids += fmt.Sprintf(" frame=%d", *e.Frame)
	}
	return fmt.Sprintf("%s%s%s: %s", e.Code, where, ids, e.Message)
}

func (e *Error) WithDetail(key string, value any) *Error {
	if e.Detail == nil {
		e.Detail = map[string]any{}
	}
	e.Detail[key] = value
	return e
}

func NewError(code ErrorCode, msg string) *Error {
	return &Error{Code: code, Message: msg}
}

func AsError(err error) (*Error, bool) {
	if err == nil {
		return nil, false
	}
	if e, ok := err.(*Error); ok {
		return e, true
	}
	return nil, false
}
