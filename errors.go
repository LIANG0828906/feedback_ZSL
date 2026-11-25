package dressbatch

import (
	"errors"
	"fmt"
	"strings"
)

const (
	CodeSchema    = "E_SCHEMA"
	CodeAssembly  = "E_ASSEMBLY"
	CodePack      = "E_PACK"
	CodeDiff      = "E_DIFF"
	CodeTruth     = "E_TRUTH"
	CodeStore     = "E_STORE"
	CodeRecovery  = "E_RECOVERY"
	CodeScheduler = "E_SCHEDULER"
	CodeHTTP      = "E_HTTP"
)

// DomainError is the stable, machine-readable error returned by the component.
// ObjectID and Field identify the exact object and field involved in a failure.
type DomainError struct {
	Code     string         `json:"code"`
	ObjectID string         `json:"object_id,omitempty"`
	Field    string         `json:"field,omitempty"`
	Message  string         `json:"message"`
	Details  map[string]any `json:"details,omitempty"`
}

func (e *DomainError) Error() string {
	if e == nil {
		return "<nil>"
	}
	var parts []string
	if e.ObjectID != "" {
		parts = append(parts, e.ObjectID)
	}
	if e.Field != "" {
		parts = append(parts, e.Field)
	}
	location := strings.Join(parts, ".")
	if location == "" {
		return fmt.Sprintf("%s: %s", e.Code, e.Message)
	}
	return fmt.Sprintf("%s %s: %s", e.Code, location, e.Message)
}

func newError(code, objectID, field, format string, args ...any) *DomainError {
	return &DomainError{
		Code:     code,
		ObjectID: objectID,
		Field:    field,
		Message:  fmt.Sprintf(format, args...),
	}
}

func withDetails(err *DomainError, details map[string]any) *DomainError {
	err.Details = details
	return err
}

func ErrorCode(err error) string {
	var domainErr *DomainError
	if errors.As(err, &domainErr) {
		return domainErr.Code
	}
	return ""
}

func AsDomainError(err error) (*DomainError, bool) {
	var domainErr *DomainError
	if errors.As(err, &domainErr) {
		return domainErr, true
	}
	return nil, false
}
