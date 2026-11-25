// Package errs defines the stable audit error codes used across the kernel.
package errs

import (
	"errors"
	"fmt"
)

const (
	// EFormat means the input is not a supported distribution format
	// (malformed envelope, unknown format number, invalid JSON, ...).
	EFormat = "E_FORMAT"
	// ESchema means a known format is missing required fields or carries
	// field-level semantic violations.
	ESchema = "E_SCHEMA"
	// EOvf means checked integer conversion/arithmetic overflowed.
	EOvf = "E_OVF"
	// EConflict means deterministic adjudication was impossible.
	EConflict = "E_CONFLICT"
)

// AuditError is the structured error returned by every public boundary.
type AuditError struct {
	Code    string `json:"code"`
	Path    string `json:"path,omitempty"`
	EventID string `json:"event_id,omitempty"`
	Message string `json:"message"`
}

func (e *AuditError) Error() string {
	switch {
	case e.Path != "" && e.EventID != "":
		return fmt.Sprintf("%s at %s (event=%s): %s", e.Code, e.Path, e.EventID, e.Message)
	case e.Path != "":
		return fmt.Sprintf("%s at %s: %s", e.Code, e.Path, e.Message)
	case e.EventID != "":
		return fmt.Sprintf("%s (event=%s): %s", e.Code, e.EventID, e.Message)
	default:
		return fmt.Sprintf("%s: %s", e.Code, e.Message)
	}
}

// New builds an AuditError.
func New(code, path, eventID, format string, args ...any) *AuditError {
	return &AuditError{
		Code:    code,
		Path:    path,
		EventID: eventID,
		Message: fmt.Sprintf(format, args...),
	}
}

// WithEvent returns a copy carrying the given event identifier.
func (e *AuditError) WithEvent(id string) *AuditError {
	cp := *e
	cp.EventID = id
	return &cp
}

// CodeOf extracts the stable code from an error ("" when absent).
func CodeOf(err error) string {
	var ae *AuditError
	if errors.As(err, &ae) {
		return ae.Code
	}
	return ""
}

// AsAudit exposes the structured error.
func AsAudit(err error) (*AuditError, bool) {
	var ae *AuditError
	if errors.As(err, &ae) {
		return ae, true
	}
	return nil, false
}
