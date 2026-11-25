package apperr

import "fmt"

const (
	CodeSchema = "E_SCHEMA"
	CodeGeom   = "E_GEOM"
	CodeDiff   = "E_DIFF"
	CodeIO     = "E_IO"
	CodeCancel = "E_CANCEL"
)

type Error struct {
	Code        string `json:"code"`
	Message     string `json:"message"`
	ZoneID      string `json:"zoneId,omitempty"`
	VertexIndex int    `json:"vertexIndex,omitempty"`
	PointIndex  int    `json:"pointIndex,omitempty"`
	PixelX      int    `json:"pixelX,omitempty"`
	PixelY      int    `json:"pixelY,omitempty"`
}

func (e *Error) Error() string {
	if e.ZoneID != "" {
		return fmt.Sprintf("%s: %s (zone=%s vertex=%d)", e.Code, e.Message, e.ZoneID, e.VertexIndex)
	}
	return fmt.Sprintf("%s: %s", e.Code, e.Message)
}

func Schema(format string, args ...any) *Error {
	return &Error{Code: CodeSchema, Message: fmt.Sprintf(format, args...)}
}

func Geom(zoneID string, vertex int, format string, args ...any) *Error {
	return &Error{
		Code:        CodeGeom,
		Message:     fmt.Sprintf(format, args...),
		ZoneID:      zoneID,
		VertexIndex: vertex,
	}
}

func Diff(format string, args ...any) *Error {
	return &Error{Code: CodeDiff, Message: fmt.Sprintf(format, args...)}
}
