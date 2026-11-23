package spatialpick

import (
	"errors"
	"fmt"
	"strings"
)

const (
	CodeSchema      = "E_SCHEMA"
	CodeCycle       = "E_CYCLE"
	CodeRef         = "E_REF"
	CodeParameter   = "E_PARA"
	CodeStale       = "E_STALE"
	CodeAmbiguous   = "E_AMBIG"
	CodeDependency  = "E_DEP"
	CodeDivergence  = "E_DIVERGE"
	CodeTransaction = "E_TXN"
	CodeNumeric     = "E_NUMERIC"
	CodeResource    = "E_RESOURCE"
	CodeCorrupt     = "E_CORRUPT"
)

// Ambiguity identifies one decision that cannot be made reliably at the
// declared fixed-point precision.
type Ambiguity struct {
	QueryID    string  `json:"query_id"`
	NodeID     string  `json:"node_id"`
	GeometryID string  `json:"geometry_id"`
	Depth      int     `json:"depth"`
	Reason     string  `json:"reason"`
	Bound      float64 `json:"bound"`
}

// CodedError is the public error protocol of the component. Fields are kept
// explicit so callers can route errors without parsing strings.
type CodedError struct {
	Code               string      `json:"code"`
	Op                 string      `json:"op,omitempty"`
	NodeID             string      `json:"node_id,omitempty"`
	Field              string      `json:"field,omitempty"`
	QueryID            string      `json:"query_id,omitempty"`
	GeometryID         string      `json:"geometry_id,omitempty"`
	BatchID            string      `json:"batch_id,omitempty"`
	ObjectID           string      `json:"object_id,omitempty"`
	DataItem           string      `json:"data_item,omitempty"`
	Depth              int         `json:"depth,omitempty"`
	ExpectedVersion    uint64      `json:"expected_version,omitempty"`
	HasExpectedVersion bool        `json:"has_expected_version,omitempty"`
	Missing            []string    `json:"missing,omitempty"`
	Ambiguities        []Ambiguity `json:"ambiguities,omitempty"`
	Left               any         `json:"left,omitempty"`
	Right              any         `json:"right,omitempty"`
	Message            string      `json:"message,omitempty"`
	Cause              error       `json:"-"`
}

func (e *CodedError) Error() string {
	if e == nil {
		return "<nil>"
	}
	var b strings.Builder
	b.WriteString(e.Code)
	if e.Op != "" {
		b.WriteString(" op=")
		b.WriteString(e.Op)
	}
	if e.NodeID != "" {
		b.WriteString(" node=")
		b.WriteString(e.NodeID)
	}
	if e.Field != "" {
		b.WriteString(" field=")
		b.WriteString(e.Field)
	}
	if e.QueryID != "" {
		b.WriteString(" query=")
		b.WriteString(e.QueryID)
	}
	if e.GeometryID != "" {
		b.WriteString(" geometry=")
		b.WriteString(e.GeometryID)
	}
	if e.BatchID != "" {
		b.WriteString(" batch=")
		b.WriteString(e.BatchID)
	}
	if e.ObjectID != "" {
		b.WriteString(" object=")
		b.WriteString(e.ObjectID)
	}
	if e.DataItem != "" {
		b.WriteString(" item=")
		b.WriteString(e.DataItem)
	}
	if e.HasExpectedVersion {
		b.WriteString(fmt.Sprintf(" expected_version=%d", e.ExpectedVersion))
	}
	if len(e.Missing) > 0 {
		b.WriteString(" missing=")
		b.WriteString(strings.Join(e.Missing, ","))
	}
	if e.Left != nil || e.Right != nil {
		b.WriteString(fmt.Sprintf(" left=%v right=%v", e.Left, e.Right))
	}
	if e.Message != "" {
		b.WriteString(": ")
		b.WriteString(e.Message)
	}
	if e.Cause != nil {
		b.WriteString(": ")
		b.WriteString(e.Cause.Error())
	}
	return b.String()
}

func (e *CodedError) Unwrap() error { return e.Cause }

func isCode(err error, code string) bool {
	var ce *CodedError
	return errors.As(err, &ce) && ce.Code == code
}

// IsCode reports whether err is a CodedError with the supplied code.
func IsCode(err error, code string) bool { return isCode(err, code) }

func coded(code, op, msg string) *CodedError {
	return &CodedError{Code: code, Op: op, Message: msg}
}
