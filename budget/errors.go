package budget

import (
	"encoding/json"
	"fmt"
)

// ErrorCode is the stable machine-readable failure classification.
type ErrorCode string

const (
	ECodeSchema ErrorCode = "E_SCHEMA"
	ECodeOVF    ErrorCode = "E_OVF"
	ECodeEval   ErrorCode = "E_EVAL"
	ECodeARB    ErrorCode = "E_ARB"
	ECodeDecode ErrorCode = "E_DECODE"
	ECodeOrder  ErrorCode = "E_ORDER"
	ECodeScale  ErrorCode = "E_SCALE"
	ECodeStage  ErrorCode = "E_STAGE"
	ECodeDiff   ErrorCode = "E_DIFF"
	ECodeIO     ErrorCode = "E_IO"
)

// Error is the only error shape crossing package/CLI boundaries.
//
// JSON serialization keeps diagnostics, tests and the local HTML page on the
// same source of truth. Optional locators are omitted when not applicable.
type Error struct {
	Code          ErrorCode        `json:"code"`
	Message       string           `json:"message"`
	Field         string           `json:"field,omitempty"`
	Path          string           `json:"path,omitempty"`
	SourceVersion SourceVersion    `json:"source_version,omitempty"`
	Tick          *int             `json:"tick,omitempty"`
	SkillID       string           `json:"skill_id,omitempty"`
	Node          string           `json:"node,omitempty"`
	Operands      []int64          `json:"operands,omitempty"`
	Arguments     map[string]int64 `json:"arguments,omitempty"`
	Contested     []string         `json:"contested_skills,omitempty"`
	Expected      any              `json:"expected,omitempty"`
	Actual        any              `json:"actual,omitempty"`
	Cause         string           `json:"cause,omitempty"`
	Basis         string           `json:"basis,omitempty"`
}

func (e *Error) Error() string {
	data, err := json.Marshal(e)
	if err != nil {
		return fmt.Sprintf("%s: %s", e.Code, e.Message)
	}
	return string(data)
}

func (e *Error) Unwrap() error { return nil }

func newError(code ErrorCode, format string, args ...any) *Error {
	return &Error{Code: code, Message: fmt.Sprintf(format, args...)}
}

func schemaError(version SourceVersion, path, format string, args ...any) *Error {
	return &Error{
		Code:          ECodeSchema,
		Message:       fmt.Sprintf(format, args...),
		Path:          path,
		Field:         path,
		SourceVersion: version,
	}
}

func withTickSkill(err *Error, tick int, skillID string) *Error {
	cp := *err
	cp.Tick = &tick
	cp.SkillID = skillID
	return &cp
}

// AsError extracts *Error from an arbitrary error chain.
func AsError(err error) (*Error, bool) {
	if err == nil {
		return nil, false
	}
	var be *Error
	if ok := errorAs(err, &be); ok {
		return be, true
	}
	return nil, false
}

func errorAs(err error, target **Error) bool {
	for err != nil {
		if e, ok := err.(*Error); ok {
			*target = e
			return true
		}
		type unwrapper interface{ Unwrap() error }
		u, ok := err.(unwrapper)
		if !ok {
			return false
		}
		err = u.Unwrap()
	}
	return false
}
