package workbench

import (
	"fmt"
	"strings"
)

const (
	CodeSchema  = "E_SCHEMA"
	CodeGeom    = "E_GEOM"
	CodeSample  = "E_SAMPLE"
	CodeDiverge = "E_DIVERGE"
	CodeBias    = "E_BIAS"
	CodeStore   = "E_STORE"
	CodeIO      = "E_IO"
)

// Error is the stable, machine-readable error envelope used by both the CLI
// and the Wails API. Field and ObjectID are intentionally separate: schema
// errors point at a JSON path, while geometry/sample errors identify a model
// object.
type Error struct {
	Code     string         `json:"code"`
	Message  string         `json:"message"`
	Field    string         `json:"field,omitempty"`
	ObjectID string         `json:"object_id,omitempty"`
	ZoneID   string         `json:"zone_id,omitempty"`
	DataItem string         `json:"data_item,omitempty"`
	Point    *PointRef      `json:"point,omitempty"`
	Value    *float64       `json:"value,omitempty"`
	Details  map[string]any `json:"details,omitempty"`
}

type PointRef struct {
	X float64 `json:"x"`
	Y float64 `json:"y"`
}

func (e *Error) Error() string {
	parts := []string{e.Code, e.Message}
	if e.Field != "" {
		parts = append(parts, "field="+e.Field)
	}
	if e.ObjectID != "" {
		parts = append(parts, "object="+e.ObjectID)
	}
	if e.ZoneID != "" {
		parts = append(parts, "zone="+e.ZoneID)
	}
	if e.DataItem != "" {
		parts = append(parts, "item="+e.DataItem)
	}
	if e.Point != nil {
		parts = append(parts, fmt.Sprintf("point=(%.6f,%.6f)", e.Point.X, e.Point.Y))
	}
	if e.Value != nil {
		parts = append(parts, fmt.Sprintf("value=%.6f", *e.Value))
	}
	return strings.Join(parts, ": ")
}

func schemaError(field, format string, args ...any) *Error {
	return &Error{Code: CodeSchema, Field: field, Message: fmt.Sprintf(format, args...)}
}

func geomError(objectID, format string, args ...any) *Error {
	return &Error{Code: CodeGeom, ObjectID: objectID, Message: fmt.Sprintf(format, args...)}
}

func sampleError(objectID string, p Vec2, format string, args ...any) *Error {
	return &Error{
		Code:     CodeSample,
		ObjectID: objectID,
		Point:    &PointRef{X: p.X, Y: p.Y},
		Message:  fmt.Sprintf(format, args...),
	}
}

func storeError(format string, args ...any) *Error {
	return &Error{Code: CodeStore, Message: fmt.Sprintf(format, args...)}
}

func ioError(format string, args ...any) *Error {
	return &Error{Code: CodeIO, Message: fmt.Sprintf(format, args...)}
}
