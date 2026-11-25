package dialogue

import "fmt"

// Business error codes used by the dialogue kernel and diagnostics.
const (
	CodeSchema = "E_SCHEMA"
	CodeLossy  = "E_LOSSY"
	CodeOvf    = "E_OVF"
	CodeLoud   = "E_LOUD"
	CodeState  = "E_STATE"
	CodeDecode = "E_DECODE"
	CodeOver   = "E_OVER"
	CodePara   = "E_PARA"
	CodeStage  = "E_STAGE"
)

// Error is the stable machine-readable error type. Field paths use JSON-path
// -like notation. Operands always contain the two integer operands involved in
// an overflow or scale contradiction when applicable.
type Error struct {
	Code      string  `json:"code"`
	Message   string  `json:"message"`
	Path      string  `json:"path,omitempty"`
	Field     string  `json:"field,omitempty"`
	SourceID  string  `json:"source_id,omitempty"`
	ClipID    string  `json:"clip_id,omitempty"`
	Operands  []int64 `json:"operands,omitempty"`
	FromState State   `json:"from_state,omitempty"`
	ToState   State   `json:"to_state,omitempty"`
	Details   any     `json:"details,omitempty"`
}

func (e *Error) Error() string {
	where := e.SourceID
	if e.Path != "" {
		if where != "" {
			where += " "
		}
		where += e.Path
	}
	if e.ClipID != "" {
		if where != "" {
			where += " "
		}
		where += "clip=" + e.ClipID
	}
	if where == "" {
		return fmt.Sprintf("%s: %s", e.Code, e.Message)
	}
	return fmt.Sprintf("%s: %s (%s)", e.Code, e.Message, where)
}

func newError(code, msg string) *Error {
	return &Error{Code: code, Message: msg}
}

func schemaError(sourceID, path, format string, args ...any) *Error {
	return &Error{
		Code:     CodeSchema,
		Message:  fmt.Sprintf(format, args...),
		Path:     path,
		Field:    path,
		SourceID: sourceID,
	}
}

func overflowError(field string, a, b int64) *Error {
	return &Error{
		Code:     CodeOvf,
		Message:  "integer conversion overflow",
		Field:    field,
		Path:     "$." + field,
		Operands: []int64{a, b},
	}
}

func parameterError(clipID, path, format string, args ...any) *Error {
	return &Error{
		Code:    CodePara,
		Message: fmt.Sprintf(format, args...),
		Path:    path,
		Field:   path,
		ClipID:  clipID,
	}
}
