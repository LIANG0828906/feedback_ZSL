package domain

import (
	"errors"
	"fmt"
	"strings"
)

const (
	CodeSchema = "E_SCHEMA"
	CodeLossy  = "E_LOSSY"
	CodeOvf    = "E_OVF"
	CodeLip    = "E_LIP"
	CodeRead   = "E_READ"
	CodeDiff   = "E_DIFF"
	CodeOrder  = "E_ORDER"
	CodeRate   = "E_RATE"
	CodeStage  = "E_STAGE"
)

// Error is the structured, machine-readable failure used by loaders,
// scheduling, rendering, checkpoint recovery and the diagnostic API.
type Error struct {
	Code           string   `json:"code"`
	Message        string   `json:"message"`
	Path           string   `json:"path,omitempty"`
	Field          string   `json:"field,omitempty"`
	DialogueID     string   `json:"dialogue_id,omitempty"`
	PartitionID    string   `json:"partition_id,omitempty"`
	PartitionIndex int      `json:"partition_index,omitempty"`
	Frame          int      `json:"frame,omitempty"`
	SequenceFrame  int      `json:"sequence_frame,omitempty"`
	LeftCategory   string   `json:"left_category,omitempty"`
	RightCategory  string   `json:"right_category,omitempty"`
	Operands       []int64  `json:"operands,omitempty"`
	Conflicts      []string `json:"conflicts,omitempty"`
	Expected       any      `json:"expected,omitempty"`
	Actual         any      `json:"actual,omitempty"`
}

func (e *Error) Error() string {
	var b strings.Builder
	b.WriteString(e.Code)
	if e.Path != "" {
		fmt.Fprintf(&b, " path=%s", e.Path)
	}
	if e.DialogueID != "" {
		fmt.Fprintf(&b, " dialogue=%s", e.DialogueID)
	}
	if e.PartitionID != "" {
		fmt.Fprintf(&b, " partition=%s", e.PartitionID)
	}
	if e.Frame != 0 || e.SequenceFrame != 0 {
		fmt.Fprintf(&b, " frame=%d", e.Frame)
	}
	if e.Message != "" {
		b.WriteString(": ")
		b.WriteString(e.Message)
	}
	return b.String()
}

func (e *Error) WithPartition(dialogueID, partitionID string, index int) *Error {
	e.DialogueID = dialogueID
	e.PartitionID = partitionID
	e.PartitionIndex = index
	return e
}

func AsError(err error) (*Error, bool) {
	var de *Error
	if errors.As(err, &de) {
		return de, true
	}
	return nil, false
}

func SchemaError(path, format string, args ...any) *Error {
	return &Error{Code: CodeSchema, Path: path, Message: fmt.Sprintf(format, args...)}
}

func OvfError(field string, operands []int64, format string, args ...any) *Error {
	return &Error{Code: CodeOvf, Field: field, Path: field, Operands: operands, Message: fmt.Sprintf(format, args...)}
}

func LipError(format string, args ...any) *Error {
	return &Error{Code: CodeLip, Message: fmt.Sprintf(format, args...)}
}

func ReadError(conflicts []string, format string, args ...any) *Error {
	return &Error{Code: CodeRead, Conflicts: conflicts, Message: fmt.Sprintf(format, args...)}
}

func DiffError(dialogueID string, partitionIndex, frame int, field string, expected, actual any) *Error {
	return &Error{
		Code:           CodeDiff,
		DialogueID:     dialogueID,
		PartitionIndex: partitionIndex,
		Frame:          frame,
		Field:          field,
		Expected:       expected,
		Actual:         actual,
		Message:        fmt.Sprintf("first mismatch at dialogue=%s partition=%d frame=%d field=%s", dialogueID, partitionIndex, frame, field),
	}
}

func StageError(format string, args ...any) *Error {
	return &Error{Code: CodeStage, Message: fmt.Sprintf(format, args...)}
}
