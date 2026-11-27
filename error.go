package grid

import "fmt"

// ErrorCode is one of the stable diagnostic codes used by the domain and API.
type ErrorCode string

const (
	CodeDecode   ErrorCode = "E_DECODE"
	CodeLossy    ErrorCode = "E_LOSSY"
	CodeRound    ErrorCode = "E_ROUND"
	CodeOVF      ErrorCode = "E_OVF"
	CodeBounce   ErrorCode = "E_BOUNCE"
	CodeCover    ErrorCode = "E_COVER"
	CodeDiff     ErrorCode = "E_DIFF"
	CodeOrder    ErrorCode = "E_ORDER"
	CodeNorm     ErrorCode = "E_NORM"
	CodeBlock    ErrorCode = "E_BLOCK"
	CodeInjected ErrorCode = "E_INJECT"
)

// Error is the structured error returned by all deterministic grid operations.
type Error struct {
	Code      ErrorCode `json:"code"`
	RequestID string    `json:"requestId,omitempty"`
	ObjectID  string    `json:"objectId,omitempty"`
	Tile      *TileID   `json:"tile,omitempty"`
	Field     string    `json:"field,omitempty"`
	Message   string    `json:"message"`
	Operands  [2]int64  `json:"operands,omitempty"`
	Measured  string    `json:"measured,omitempty"`
	Expected  string    `json:"expected,omitempty"`
	OldValue  string    `json:"oldValue,omitempty"`
	BounceSeq int       `json:"bounceSeq,omitempty"`
}

func (e *Error) Error() string {
	where := ""
	if e.RequestID != "" {
		where += " request=" + e.RequestID
	}
	if e.ObjectID != "" {
		where += " object=" + e.ObjectID
	}
	if e.Field != "" {
		where += " field=" + e.Field
	}
	if e.Tile != nil {
		where += fmt.Sprintf(" tile=(%d,%d)", e.Tile.Col, e.Tile.Row)
	}
	return fmt.Sprintf("%s%s: %s", e.Code, where, e.Message)
}

func newError(code ErrorCode, msg string) *Error {
	return &Error{Code: code, Message: msg}
}

func decodeError(objectID, field, format string, args ...any) *Error {
	return &Error{Code: CodeDecode, ObjectID: objectID, Field: field, Message: fmt.Sprintf(format, args...)}
}

func overflowError(field string, a, b int64) *Error {
	return &Error{
		Code:     CodeOVF,
		Field:    field,
		Operands: [2]int64{a, b},
		Message:  "integer conversion overflow",
	}
}

// AsError returns the structured *Error and whether err has the requested code.
func AsError(err error, code ErrorCode) (*Error, bool) {
	var ge *Error
	if !errorAs(err, &ge) {
		return nil, false
	}
	if code != "" && ge.Code != code {
		return ge, false
	}
	return ge, true
}

func errorAs(err error, target any) bool {
	type asTarget interface{ As(any) bool }
	for err != nil {
		if x, ok := err.(*Error); ok {
			if ptr, ok := target.(**Error); ok {
				*ptr = x
				return true
			}
		}
		if as, ok := err.(asTarget); ok {
			if as.As(target) {
				return true
			}
		}
		type wrapper interface{ Unwrap() error }
		w, ok := err.(wrapper)
		if !ok {
			return false
		}
		err = w.Unwrap()
	}
	return false
}
