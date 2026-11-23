package terrainbake

import (
	"encoding/json"
	"fmt"
)

// Business error codes. Callers should switch on Code and may inspect the
// structured fields to identify the failing object, field, block or pixel.
const (
	CodeSchema   = "E_SCHEMA"
	CodeParam    = "E_PARA"
	CodeSeam     = "E_SEAM"
	CodeDiff     = "E_DIFF"
	CodeResource = "E_RESOURCE"
	CodeState    = "E_STATE"
	CodeIO       = "E_IO"
	CodeBake     = "E_BAKE"
)

// Error is the single structured error returned by the public API. It is
// deliberately JSON-friendly so the CLI and diagnostics tools can emit the
// same representation.
type Error struct {
	Code    string         `json:"code"`
	Object  string         `json:"object,omitempty"`
	Field   string         `json:"field,omitempty"`
	BlockID string         `json:"block_id,omitempty"`
	BlockA  string         `json:"block_a,omitempty"`
	BlockB  string         `json:"block_b,omitempty"`
	SeamID  string         `json:"seam_id,omitempty"`
	X       int            `json:"x"`
	Y       int            `json:"y"`
	Angle   float64        `json:"angle_deg,omitempty"`
	Limit   float64        `json:"limit,omitempty"`
	Message string         `json:"message"`
	Details map[string]any `json:"details,omitempty"`
}

func (e *Error) Error() string {
	if e == nil {
		return ""
	}
	return fmt.Sprintf("%s: %s", e.Code, e.Message)
}

// AsError extracts a structured *Error from an error chain.
func AsError(err error) (*Error, bool) {
	for err != nil {
		if e, ok := err.(*Error); ok {
			return e, true
		}
		type unwrapper interface{ Unwrap() error }
		u, ok := err.(unwrapper)
		if !ok {
			break
		}
		err = u.Unwrap()
	}
	return nil, false
}

// MarshalJSON keeps the x/y fields out of errors where they carry no meaning.
func (e Error) MarshalJSON() ([]byte, error) {
	type alias Error
	m := map[string]any{
		"code":    e.Code,
		"message": e.Message,
	}
	if e.Object != "" {
		m["object"] = e.Object
	}
	if e.Field != "" {
		m["field"] = e.Field
	}
	if e.BlockID != "" {
		m["block_id"] = e.BlockID
	}
	if e.BlockA != "" {
		m["block_a"] = e.BlockA
	}
	if e.BlockB != "" {
		m["block_b"] = e.BlockB
	}
	if e.SeamID != "" {
		m["seam_id"] = e.SeamID
	}
	if e.X != 0 || e.Y != 0 || e.Code == CodeDiff || e.Field == "normal_slope" || e.Field == "heights" {
		m["x"] = e.X
		m["y"] = e.Y
	}
	if e.Angle != 0 {
		m["angle_deg"] = e.Angle
	}
	if e.Limit != 0 {
		m["limit"] = e.Limit
	}
	if len(e.Details) != 0 {
		m["details"] = e.Details
	}
	return json.Marshal(m)
}
