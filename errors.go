package terrain

import (
	"errors"
	"fmt"
)

const (
	CodeSchema = "E_SCHEMA"
	CodeAlign  = "E_ALIGN"
	CodeDiff   = "E_DIFF"
	CodeBudget = "E_BUDGET"
)

type CodedError struct {
	Code    string         `json:"code"`
	Path    string         `json:"path,omitempty"`
	BlockNo *int           `json:"block_no,omitempty"`
	Message string         `json:"message"`
	Details map[string]any `json:"details,omitempty"`
}

func (e *CodedError) Error() string {
	where := e.Code
	if e.Path != "" {
		where += " at " + e.Path
	}
	if e.BlockNo != nil {
		where += fmt.Sprintf(" (block %d)", *e.BlockNo)
	}
	return fmt.Sprintf("%s: %s: %s", where, e.Code, e.Message)
}

func NewSchemaError(path string, blockNo *int, format string, args ...any) error {
	return &CodedError{Code: CodeSchema, Path: path, BlockNo: blockNo, Message: fmt.Sprintf(format, args...)}
}

func AsCodedError(err error) (*CodedError, bool) {
	var coded *CodedError
	if errors.As(err, &coded) {
		return coded, true
	}
	return nil, false
}

type AlignError struct {
	BlockA        int     `json:"block_a"`
	BlockB        int     `json:"block_b"`
	MeasuredAngle float64 `json:"measured_fold_angle_deg"`
	Reason        string  `json:"reason"`
}

func (e *AlignError) Error() string {
	return fmt.Sprintf("%s: cannot align blocks %d and %d; measured fold angle %.3f deg: %s",
		CodeAlign, e.BlockA, e.BlockB, e.MeasuredAngle, e.Reason)
}

func IsAlignError(err error) bool {
	var alignErr *AlignError
	return errors.As(err, &alignErr)
}

type DiffLocation struct {
	BlockNo       int    `json:"block_no"`
	VertexIndex   *int   `json:"vertex_index,omitempty"`
	TriangleIndex *int   `json:"triangle_index,omitempty"`
	Field         string `json:"field"`
	Expected      any    `json:"expected,omitempty"`
	Actual        any    `json:"actual,omitempty"`
}

type DiffError struct {
	Location DiffLocation `json:"location"`
}

func (e *DiffError) Error() string {
	loc := fmt.Sprintf("block %d field %s", e.Location.BlockNo, e.Location.Field)
	if e.Location.VertexIndex != nil {
		loc += fmt.Sprintf(" vertex %d", *e.Location.VertexIndex)
	}
	if e.Location.TriangleIndex != nil {
		loc += fmt.Sprintf(" triangle %d", *e.Location.TriangleIndex)
	}
	return fmt.Sprintf("%s: first inconsistency at %s", CodeDiff, loc)
}

func IsDiffError(err error) bool {
	var diffErr *DiffError
	return errors.As(err, &diffErr)
}
