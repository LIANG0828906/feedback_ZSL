package maskqa

import "fmt"

type ErrorCode string

const (
	ESchema  ErrorCode = "E_SCHEMA"
	EPara    ErrorCode = "E_PARA"
	ERadius  ErrorCode = "E_RADIUS"
	EIter    ErrorCode = "E_ITER"
	EState   ErrorCode = "E_STATE"
	EDiff    ErrorCode = "E_DIFF"
	ECache   ErrorCode = "E_CACHE"
	EStorage ErrorCode = "E_STORAGE"
)

type QAError struct {
	Code       ErrorCode      `json:"code"`
	Field      string         `json:"field,omitempty"`
	ObjectID   string         `json:"object_id,omitempty"`
	Message    string         `json:"message"`
	Actual     any            `json:"actual,omitempty"`
	Min        any            `json:"min,omitempty"`
	Max        any            `json:"max,omitempty"`
	Round      int            `json:"round,omitempty"`
	MinChanges int            `json:"min_changes,omitempty"`
	Detail     map[string]any `json:"detail,omitempty"`
}

func (e *QAError) Error() string {
	if e == nil {
		return ""
	}
	if e.Field != "" || e.ObjectID != "" {
		return fmt.Sprintf("%s field=%s object=%s: %s", e.Code, e.Field, e.ObjectID, e.Message)
	}
	return fmt.Sprintf("%s: %s", e.Code, e.Message)
}

func schemaError(field, objectID, format string, args ...any) error {
	return &QAError{Code: ESchema, Field: field, ObjectID: objectID, Message: fmt.Sprintf(format, args...)}
}

func paraError(field string, min, max, actual any, format string, args ...any) error {
	return &QAError{Code: EPara, Field: field, Message: fmt.Sprintf(format, args...), Actual: actual, Min: min, Max: max}
}

func stateError(from, to State) error {
	return &QAError{
		Code:    EState,
		Field:   "state",
		Message: fmt.Sprintf("illegal state transition %s -> %s", from, to),
		Actual:  map[string]string{"from": string(from), "to": string(to)},
	}
}

type Difference struct {
	SampleID string     `json:"sample_id"`
	Block    BlockCoord `json:"block"`
	Stage    Stage      `json:"stage"`
	Field    string     `json:"field"`
	Index    int        `json:"index,omitempty"`
	X        int        `json:"x,omitempty"`
	Y        int        `json:"y,omitempty"`
	Expected any        `json:"expected,omitempty"`
	Actual   any        `json:"actual,omitempty"`
	Message  string     `json:"message"`
}

type DiffError struct {
	Code        ErrorCode    `json:"code"`
	First       Difference   `json:"first"`
	Differences []Difference `json:"differences,omitempty"`
}

func (e *DiffError) Error() string {
	return fmt.Sprintf("%s first mismatch %s %s %s: %s", EDiff, e.First.Block.ID(), e.First.Stage, e.First.Field, e.First.Message)
}
