package acousticfield

import (
	"errors"
	"fmt"
	"strings"
)

// ErrorCode is the stable machine-readable error category exposed by the
// domain component.
type ErrorCode string

const (
	CodeSchema ErrorCode = "E_SCHEMA"
	CodeValue  ErrorCode = "E_VAL"
	CodeCons   ErrorCode = "E_CONS"
	CodeState  ErrorCode = "E_STATE"
	CodeDiff   ErrorCode = "E_DIFF"
)

// FieldError carries enough context for callers to identify the exact object
// and field that failed validation or consistency checking.
type FieldError struct {
	Code       ErrorCode
	ObjectID   string
	FieldPath  string
	Measured   any
	Min        any
	Max        any
	BandID     string
	SourceID   string
	Values     map[string]int64
	Diff       *Diff
	Message    string
	Underlying error
}

func (e *FieldError) Error() string {
	if e == nil {
		return ""
	}
	var b strings.Builder
	b.WriteString(string(e.Code))
	if e.ObjectID != "" {
		b.WriteString(" object=")
		b.WriteString(e.ObjectID)
	}
	if e.FieldPath != "" {
		b.WriteString(" field=")
		b.WriteString(e.FieldPath)
	}
	if e.BandID != "" {
		b.WriteString(" band=")
		b.WriteString(e.BandID)
	}
	if e.SourceID != "" {
		b.WriteString(" source=")
		b.WriteString(e.SourceID)
	}
	if e.Measured != nil {
		b.WriteString(fmt.Sprintf(" measured=%v", e.Measured))
	}
	if e.Min != nil || e.Max != nil {
		b.WriteString(fmt.Sprintf(" allowed=[%v,%v]", e.Min, e.Max))
	}
	if len(e.Values) > 0 {
		keys := make([]string, 0, len(e.Values))
		for k := range e.Values {
			keys = append(keys, k)
		}
		sortStrings(keys)
		for _, k := range keys {
			b.WriteString(fmt.Sprintf(" %s=%d", k, e.Values[k]))
		}
	}
	if e.Diff != nil {
		b.WriteString(e.Diff.String())
	}
	if e.Message != "" {
		b.WriteString(": ")
		b.WriteString(e.Message)
	}
	if e.Underlying != nil {
		b.WriteString(": ")
		b.WriteString(e.Underlying.Error())
	}
	return b.String()
}

func (e *FieldError) Unwrap() error { return e.Underlying }

// IsCode reports whether err is a FieldError with code.
func IsCode(err error, code ErrorCode) bool {
	var fe *FieldError
	return errors.As(err, &fe) && fe.Code == code
}

func schemaError(objectID, fieldPath, message string) *FieldError {
	return &FieldError{Code: CodeSchema, ObjectID: objectID, FieldPath: fieldPath, Message: message}
}

func valueError(objectID, fieldPath string, measured, min, max any) *FieldError {
	return &FieldError{
		Code:      CodeValue,
		ObjectID:  objectID,
		FieldPath: fieldPath,
		Measured:  measured,
		Min:       min,
		Max:       max,
	}
}

// Diff identifies the first non-equivalent field. It is used by the
// independent verifier and by three-path comparisons.
type Diff struct {
	ChunkID  int
	BandID   string
	Field    string
	Expected any
	Actual   any
	RunA     string
	RunB     string
	Sequence int
	PathID   string
	SourceID string
	Reason   string
}

func (d *Diff) String() string {
	if d == nil {
		return ""
	}
	var b strings.Builder
	b.WriteString(" first-diff")
	if d.Sequence > 0 {
		b.WriteString(fmt.Sprintf(" sequence=%d", d.Sequence))
	}
	if d.ChunkID >= 0 {
		b.WriteString(fmt.Sprintf(" chunk=%d", d.ChunkID))
	}
	if d.BandID != "" {
		b.WriteString(" band=" + d.BandID)
	}
	if d.Field != "" {
		b.WriteString(" field=" + d.Field)
	}
	if d.PathID != "" {
		b.WriteString(" path=" + d.PathID)
	}
	if d.SourceID != "" {
		b.WriteString(" source=" + d.SourceID)
	}
	if d.RunA != "" || d.RunB != "" {
		b.WriteString(fmt.Sprintf(" runs=%s/%s", d.RunA, d.RunB))
	}
	if d.Expected != nil || d.Actual != nil {
		b.WriteString(fmt.Sprintf(" expected=%v actual=%v", d.Expected, d.Actual))
	}
	if d.Reason != "" {
		b.WriteString(" reason=" + d.Reason)
	}
	return b.String()
}

func diffError(d *Diff) *FieldError {
	return &FieldError{
		Code:     CodeDiff,
		ObjectID: fmt.Sprintf("chunk-%d", d.ChunkID),
		Diff:     d,
		Message:  "persisted or parallel-path result differs",
	}
}
