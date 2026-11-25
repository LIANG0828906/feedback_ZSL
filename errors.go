package texture

import (
	"errors"
	"fmt"
	"strings"
)

// Code is a stable machine-readable kernel error code.
type Code string

const (
	CodeVersion Code = "E_VERSION"
	CodeSchema  Code = "E_SCHEMA"
	CodePara    Code = "E_PARA"
	CodeLossy   Code = "E_LOSSY"
	CodeState   Code = "E_STATE"
	CodeStore   Code = "E_STORE"
	CodeDiff    Code = "E_DIFF"
)

// Loss describes one value that cannot be represented faithfully by a target
// plan version and the deterministic downgrade used instead.
type Loss struct {
	Path      string `json:"path"`
	Original  any    `json:"original"`
	Downgrade any    `json:"downgrade"`
	Reason    string `json:"reason"`
}

// KernelError carries the code and the exact document field path involved in
// a failure. Path is always rooted at "$" for plan/document errors.
type KernelError struct {
	Code    Code
	Path    string
	Field   string
	Message string
	Losses  []Loss
	Diff    *DiffInfo
	Err     error
}

// DiffInfo identifies the first deterministic divergence found by comparison.
type DiffInfo struct {
	X          int               `json:"x"`
	Y          int               `json:"y"`
	EntryIndex int               `json:"entry_index"`
	Field      string            `json:"field"`
	Reason     string            `json:"reason"`
	Values     map[string]string `json:"values,omitempty"`
}

func (d *DiffInfo) String() string {
	if d == nil {
		return ""
	}
	return fmt.Sprintf("first diff at (%d,%d) entry %d field %s: %s", d.X, d.Y, d.EntryIndex, d.Field, d.Reason)
}

func (e *KernelError) Error() string {
	if e == nil {
		return ""
	}
	var b strings.Builder
	b.WriteString(string(e.Code))
	if e.Path != "" {
		b.WriteString(" at ")
		b.WriteString(e.Path)
	}
	if e.Field != "" {
		b.WriteString(" field ")
		b.WriteString(e.Field)
	}
	if e.Message != "" {
		b.WriteString(": ")
		b.WriteString(e.Message)
	}
	if len(e.Losses) > 0 {
		fmt.Fprintf(&b, " (%d lossy item(s))", len(e.Losses))
	}
	if e.Diff != nil {
		fmt.Fprintf(&b, ": %s", e.Diff.String())
	}
	if e.Err != nil {
		fmt.Fprintf(&b, ": %v", e.Err)
	}
	return b.String()
}

func (e *KernelError) Unwrap() error { return e.Err }

func errf(code Code, path, field, format string, args ...any) *KernelError {
	return &KernelError{
		Code:    code,
		Path:    path,
		Field:   field,
		Message: fmt.Sprintf(format, args...),
	}
}

// HasCode reports whether err (or a wrapped error) is a KernelError with code.
func HasCode(err error, code Code) bool {
	var ke *KernelError
	return errors.As(err, &ke) && ke.Code == code
}

// AsKernelError returns a wrapped KernelError when available.
func AsKernelError(err error) (*KernelError, bool) {
	var ke *KernelError
	if errors.As(err, &ke) {
		return ke, true
	}
	return nil, false
}

// ErrCanceled is returned when a run stops before all blocks are committed.
// Committed blocks remain valid and resumable.
var ErrCanceled = errors.New("texture run canceled")
