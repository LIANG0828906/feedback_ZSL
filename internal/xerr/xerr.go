// Package xerr defines the stable error codes used by the wardrobe kernel.
package xerr

import (
	"encoding/json"
	"errors"
	"fmt"
)

const (
	// ESchema means an input document failed structural validation.
	ESchema = "E_SCHEMA"
	// EArb means deterministic arbitration could not choose a winner.
	EArb = "E_ARB"
	// EDiff means two execution paths produced different results.
	EDiff = "E_DIFF"
	// EVer means persisted checkpoint/cache versions are incompatible.
	EVer = "E_VER"
	// EBudget means a single part cannot fit even an empty batch.
	EBudget = "E_BUDGET"
	// EState means the kernel reached an invalid in-memory/persisted state.
	EState = "E_STATE"
	// EIO means filesystem or serialization I/O failed.
	EIO = "E_IO"
)

// Violation is one field-level schema problem.
type Violation struct {
	Code      string `json:"code"`
	PartID    string `json:"part_id,omitempty"`
	FieldPath string `json:"field_path,omitempty"`
	Message   string `json:"message"`
}

// Mismatch locates the first difference between incremental and full paths.
type Mismatch struct {
	BatchNumber int    `json:"batch_number"`
	PartID      string `json:"part_id,omitempty"`
	FieldName   string `json:"field_name"`
	Path        string `json:"path,omitempty"`
	Expected    any    `json:"expected,omitempty"`
	Actual      any    `json:"actual,omitempty"`
}

// Error is the serializable domain error returned by all public entry points.
type Error struct {
	Code             string      `json:"code"`
	Message          string      `json:"message"`
	Violations       []Violation `json:"violations,omitempty"`
	Parties          []string    `json:"parties,omitempty"`
	BasisChain       []string    `json:"basis_chain,omitempty"`
	FirstMismatch    *Mismatch   `json:"first_mismatch,omitempty"`
	AffectedBatches  []int       `json:"affected_batches,omitempty"`
	ExpectedVersion  string      `json:"expected_version,omitempty"`
	PersistedVersion string      `json:"persisted_version,omitempty"`
	Retryable        bool        `json:"retryable,omitempty"`
	Cause            error       `json:"-"`
}

func (e *Error) Error() string {
	if e == nil {
		return ""
	}
	if e.Message != "" {
		return fmt.Sprintf("%s: %s", e.Code, e.Message)
	}
	return e.Code
}

func (e *Error) Unwrap() error { return e.Cause }

// New creates a coded error.
func New(code, message string) *Error {
	return &Error{Code: code, Message: message}
}

// Wrap wraps an underlying error while preserving the stable code.
func Wrap(code, message string, cause error) *Error {
	return &Error{Code: code, Message: message, Cause: cause}
}

// AsError returns *Error when err carries one.
func AsError(err error) (*Error, bool) {
	var coded *Error
	if errors.As(err, &coded) {
		return coded, true
	}
	return nil, false
}

// JSON renders the stable machine-readable form.
func JSON(err error) []byte {
	if err == nil {
		return nil
	}
	var coded *Error
	if !errors.As(err, &coded) {
		coded = New("E_INTERNAL", err.Error())
		coded.Cause = err
	}
	data, mErr := json.MarshalIndent(coded, "", "  ")
	if mErr != nil {
		return []byte(fmt.Sprintf(`{"code":%q,"message":%q}`, coded.Code, coded.Message))
	}
	return data
}
