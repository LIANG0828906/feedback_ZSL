package silhouette

import (
	"errors"
	"fmt"
	"sort"
)

const (
	CodeSchema    = "E_SCHEMA"
	CodeLossy     = "E_LOSSY"
	CodeOverflow  = "E_OVF"
	CodeTolerance = "E_TOL"
	CodeBudget    = "E_BUDGET"
	CodeBuffer    = "E_BUFFER"

	CodeInternal = "E_INTERNAL"
)

// DomainError is a stable, machine-readable error. Details intentionally use
// strings for JSON and log friendliness while still preserving operands.
type DomainError struct {
	Code    string            `json:"code"`
	Path    string            `json:"path,omitempty"`
	Message string            `json:"message"`
	Details map[string]string `json:"details,omitempty"`
}

func (e *DomainError) Error() string {
	if e.Path == "" {
		return fmt.Sprintf("%s: %s", e.Code, e.Message)
	}
	return fmt.Sprintf("%s at %s: %s", e.Code, e.Path, e.Message)
}

func NewDomainError(code, path, message string) *DomainError {
	return &DomainError{Code: code, Path: path, Message: message}
}

func (e *DomainError) WithDetail(key, value string) *DomainError {
	if e.Details == nil {
		e.Details = make(map[string]string)
	}
	e.Details[key] = value
	return e
}

func IsCode(err error, code string) bool {
	var de *DomainError
	return errors.As(err, &de) && de.Code == code
}

// LossyIssue describes a field that cannot be represented exactly by a target
// schema. Lossy exports remain loadable, but callers can decide whether to use
// them based on this list.
type LossyIssue struct {
	Code     string `json:"code"`
	Path     string `json:"path"`
	Message  string `json:"message"`
	Source   int    `json:"source_version"`
	Target   int    `json:"target_version"`
	OldValue string `json:"old_value,omitempty"`
	NewValue string `json:"new_value,omitempty"`
}

func (l LossyIssue) Error() string {
	return fmt.Sprintf("%s at %s: %s", l.Code, l.Path, l.Message)
}

type LossyIssues []LossyIssue

func (l LossyIssues) Error() string {
	if len(l) == 0 {
		return ""
	}
	return fmt.Sprintf("%s at %s: %s (%d issue(s))", l[0].Code, l[0].Path, l[0].Message, len(l))
}

// ConversionTrace is an append-only explanation of a loader/exporter decision.
type ConversionTrace struct {
	Stage         string `json:"stage"`
	Path          string `json:"path"`
	Action        string `json:"action"`
	SourceVersion int    `json:"source_version,omitempty"`
	TargetVersion int    `json:"target_version,omitempty"`
	OldValue      string `json:"old_value,omitempty"`
	NewValue      string `json:"new_value,omitempty"`
	Unit          string `json:"unit,omitempty"`
	OperandA      string `json:"operand_a,omitempty"`
	OperandB      string `json:"operand_b,omitempty"`
	Remainder     string `json:"remainder,omitempty"`
	Truncated     bool   `json:"truncated,omitempty"`
	DefaultSource string `json:"default_source,omitempty"`
	Note          string `json:"note,omitempty"`
}

func defaultFillTrace(path string, source, target int, value, defaultSource, note string) ConversionTrace {
	return ConversionTrace{
		Stage: "load", Path: path, Action: "default_fill",
		OldValue: "<missing>", NewValue: value, SourceVersion: source, TargetVersion: target,
		DefaultSource: defaultSource, Note: note,
	}
}

func SortTraces(traces []ConversionTrace) {
	sort.SliceStable(traces, func(i, j int) bool {
		if traces[i].Path == traces[j].Path {
			return traces[i].Stage < traces[j].Stage
		}
		return traces[i].Path < traces[j].Path
	})
}
