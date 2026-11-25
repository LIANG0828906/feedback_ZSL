package macrokit

import (
	"errors"
	"fmt"
	"strings"
)

type ErrorCode string

const (
	ESchema      ErrorCode = "E_SCHEMA"
	ERef         ErrorCode = "E_REF"
	EOvf         ErrorCode = "E_OVF"
	EConflict    ErrorCode = "E_CONFLICT"
	EDiff        ErrorCode = "E_DIFF"
	EFault       ErrorCode = "E_FAULT"
	ECorrupt     ErrorCode = "E_CORRUPT"
	EVersion     ErrorCode = "E_VERSION"
	EUnsupported ErrorCode = "E_UNSUPPORTED"
)

// Error is the stable domain error returned by loading, conversion, execution,
// comparison and checkpoint recovery.
type Error struct {
	Code        ErrorCode `json:"code"`
	Message     string    `json:"message"`
	Path        string    `json:"path,omitempty"`
	Ref         string    `json:"ref,omitempty"`
	BranchID    string    `json:"branch_id,omitempty"`
	MacroID     string    `json:"macro_id,omitempty"`
	Frame       int64     `json:"frame,omitempty"`
	Field       string    `json:"field,omitempty"`
	ConflictSet []string  `json:"conflict_set,omitempty"`
	Cause       error     `json:"-"`
}

func (e *Error) Error() string {
	var b strings.Builder
	b.WriteString(string(e.Code))
	if e.Message != "" {
		b.WriteString(": ")
		b.WriteString(e.Message)
	}
	if e.Path != "" {
		fmt.Fprintf(&b, " path=%s", e.Path)
	}
	if e.Ref != "" {
		fmt.Fprintf(&b, " ref=%s", e.Ref)
	}
	if e.BranchID != "" {
		fmt.Fprintf(&b, " branch=%s", e.BranchID)
	}
	if e.Frame != 0 || e.Field != "" {
		fmt.Fprintf(&b, " at frame=%d field=%s", e.Frame, e.Field)
	}
	if len(e.ConflictSet) > 0 {
		fmt.Fprintf(&b, " conflicts=[%s]", strings.Join(e.ConflictSet, ","))
	}
	if e.Cause != nil {
		fmt.Fprintf(&b, ": %v", e.Cause)
	}
	return b.String()
}

func (e *Error) Unwrap() error { return e.Cause }

func newSchemaError(path, format string, args ...any) *Error {
	return &Error{Code: ESchema, Path: path, Message: fmt.Sprintf(format, args...)}
}

func newRefError(path, ref, format string, args ...any) *Error {
	return &Error{Code: ERef, Path: path, Ref: ref, Message: fmt.Sprintf(format, args...)}
}

func newOvfError(branchID, path, field string, value int64) *Error {
	return &Error{
		Code:     EOvf,
		BranchID: branchID,
		Path:     path,
		Field:    field,
		Message:  fmt.Sprintf("integer conversion overflow at value %d", value),
	}
}

// AsError returns the first *Error in err's wrap chain.
func AsError(err error) (*Error, bool) {
	var domainErr *Error
	if errors.As(err, &domainErr) {
		return domainErr, true
	}
	return nil, false
}

func HasCode(err error, code ErrorCode) bool {
	domainErr, ok := AsError(err)
	return ok && domainErr.Code == code
}
