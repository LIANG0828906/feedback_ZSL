package core

import (
	"errors"
	"fmt"
	"strings"
)

// Error codes are deliberately stable because callers use them as protocol
// values at process and package boundaries.
const (
	CodeSemantic  = "E_SEM"
	CodeMapping   = "E_MAP"
	CodeTime      = "E_TIME"
	CodeCondition = "E_COND"
	CodeCycle     = "E_CYCLE"
	CodeSemEq     = "E_SEMEQ"
	CodeFinger    = "E_FP"
	CodeVersion   = "E_VER"
	CodeDiff      = "E_DIFF"
	CodeConflict  = "E_CONFLICT"
)

// Error carries enough diagnostic context for a caller to identify the exact
// field, branch, window, and value involved in a failure.
type Error struct {
	Code         string
	Op           string
	Message      string
	FieldPath    string
	Conditional  string
	Branch       string
	SourceBranch string
	TargetBranch string
	Semantic     string
	Entry        string
	Conflict     string
	Measured     any
	Left         any
	Right        any
	Chain        []string
	Window       int
	Expected     string
	Actual       string
	FingerprintA string
	FingerprintB string
	EntryA       string
	EntryB       string
}

func (e *Error) Error() string {
	if e == nil {
		return "<nil>"
	}
	var parts []string
	parts = append(parts, e.Code)
	if e.Op != "" {
		parts = append(parts, e.Op)
	}
	if e.Message != "" {
		parts = append(parts, e.Message)
	}
	if e.Semantic != "" {
		parts = append(parts, "semantic="+e.Semantic)
	}
	if e.FieldPath != "" {
		parts = append(parts, "field="+e.FieldPath)
	}
	if e.Conditional != "" {
		parts = append(parts, "conditional="+e.Conditional)
	}
	if e.Branch != "" {
		parts = append(parts, "branch="+e.Branch)
	}
	if e.SourceBranch != "" || e.TargetBranch != "" {
		parts = append(parts, fmt.Sprintf("branches=%s->%s", e.SourceBranch, e.TargetBranch))
	}
	if e.Entry != "" {
		parts = append(parts, "entry="+e.Entry)
	}
	if e.Conflict != "" {
		parts = append(parts, "conflict="+e.Conflict)
	}
	if e.Measured != nil {
		parts = append(parts, fmt.Sprintf("measured=%v", e.Measured))
	}
	if e.Left != nil || e.Right != nil {
		parts = append(parts, fmt.Sprintf("left=%v right=%v", e.Left, e.Right))
	}
	if len(e.Chain) > 0 {
		parts = append(parts, "chain="+strings.Join(e.Chain, " -> "))
	}
	if e.Window >= 0 {
		parts = append(parts, fmt.Sprintf("window=%d", e.Window))
	}
	if e.Expected != "" || e.Actual != "" {
		parts = append(parts, fmt.Sprintf("expected=%s actual=%s", e.Expected, e.Actual))
	}
	if e.FingerprintA != "" || e.FingerprintB != "" {
		parts = append(parts, fmt.Sprintf("fingerprints=%s/%s", e.FingerprintA, e.FingerprintB))
	}
	if e.EntryA != "" || e.EntryB != "" {
		parts = append(parts, fmt.Sprintf("entries=%s/%s", e.EntryA, e.EntryB))
	}
	return strings.Join(parts, " ")
}

func NewError(code, op, message string) *Error {
	return &Error{Code: code, Op: op, Message: message, Window: -1}
}

func CodeOf(err error) string {
	var e *Error
	if errors.As(err, &e) {
		return e.Code
	}
	return ""
}
