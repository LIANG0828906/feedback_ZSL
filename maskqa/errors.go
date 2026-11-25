package maskqa

import (
	"errors"
	"fmt"
	"strings"
)

// ErrorCode is the stable machine-readable error vocabulary used by this workbench.
type ErrorCode string

const (
	CodeSchema ErrorCode = "E_SCHEMA"
	CodeOVF    ErrorCode = "E_OVF"
	CodePARA   ErrorCode = "E_PARA"
	CodeORDER  ErrorCode = "E_ORDER"
	CodeTOP    ErrorCode = "E_TOP"
	CodeDIR    ErrorCode = "E_DIR"
	CodeDecode ErrorCode = "E_DECODE"
	CodeDIFF   ErrorCode = "E_DIFF"
)

// DomainIssue carries topology measurement details for diagnostics.
type DomainIssue struct {
	DomainID  int     `json:"domain_id"`
	Field     string  `json:"field"`
	Expected  float64 `json:"expected"`
	Actual    float64 `json:"actual"`
	Tolerance float64 `json:"tolerance"`
	Message   string  `json:"message,omitempty"`
}

// SegmentIssue identifies a direction segment that does not match truth.
type SegmentIssue struct {
	SegmentID     string  `json:"segment_id"`
	DomainID      int     `json:"domain_id"`
	Expected      string  `json:"expected_category"`
	Actual        string  `json:"actual_category"`
	ExpectedAngle float64 `json:"expected_angle_deg"`
	MeasuredAngle float64 `json:"measured_angle_deg"`
	NormalAngle   float64 `json:"normal_angle_deg"`
}

// Error is the structured error returned across schema, arithmetic, raster and verification boundaries.
type Error struct {
	Code          ErrorCode      `json:"code"`
	Message       string         `json:"message"`
	Field         string         `json:"field,omitempty"`
	Path          string         `json:"path,omitempty"`
	SourceVersion string         `json:"source_version,omitempty"`
	Operands      []int64        `json:"operands,omitempty"`
	LegalRange    string         `json:"legal_range,omitempty"`
	Conflicts     []int          `json:"conflicts,omitempty"`
	Domains       []DomainIssue  `json:"domains,omitempty"`
	Segments      []SegmentIssue `json:"segments,omitempty"`
	FirstTile     int            `json:"first_tile,omitempty"`
	Cause         error          `json:"-"`
}

func (e *Error) Error() string {
	var b strings.Builder
	fmt.Fprintf(&b, "%s: %s", e.Code, e.Message)
	if e.Path != "" {
		fmt.Fprintf(&b, " path=%s", e.Path)
	}
	if e.Field != "" && e.Path == "" {
		fmt.Fprintf(&b, " field=%s", e.Field)
	}
	if e.SourceVersion != "" {
		fmt.Fprintf(&b, " source_version=%s", e.SourceVersion)
	}
	if len(e.Operands) > 0 {
		fmt.Fprintf(&b, " operands=%v", e.Operands)
	}
	if e.LegalRange != "" {
		fmt.Fprintf(&b, " legal=%s", e.LegalRange)
	}
	if len(e.Conflicts) > 0 {
		fmt.Fprintf(&b, " conflicts=%v", e.Conflicts)
	}
	if e.FirstTile != 0 {
		fmt.Fprintf(&b, " first_tile=%d", e.FirstTile)
	}
	if e.Cause != nil {
		fmt.Fprintf(&b, ": %v", e.Cause)
	}
	return b.String()
}

func (e *Error) Unwrap() error { return e.Cause }

// AsError returns the first *Error in err's tree, if any.
func AsError(err error) (*Error, bool) {
	var qe *Error
	if errors.As(err, &qe) {
		return qe, true
	}
	return nil, false
}

func schemaError(version, path, format string, args ...any) *Error {
	return &Error{Code: CodeSchema, SourceVersion: version, Path: path, Field: path, Message: fmt.Sprintf(format, args...)}
}

func paramError(field, legalRange, format string, args ...any) *Error {
	return &Error{Code: CodePARA, Field: field, Path: field, LegalRange: legalRange, Message: fmt.Sprintf(format, args...)}
}

func overflowError(field string, a, b int64) *Error {
	return &Error{
		Code:     CodeOVF,
		Field:    field,
		Path:     field,
		Operands: []int64{a, b},
		Message:  "integer conversion overflow",
	}
}
