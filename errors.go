package volumefog

import (
	"errors"
	"fmt"
)

// Code is a stable diagnostic code used by the library, CLI and workbench.
type Code string

const (
	E_SCHEMA  Code = "E_SCHEMA"
	E_DECODE  Code = "E_DECODE"
	E_ORDER   Code = "E_ORDER"
	E_DENS    Code = "E_DENS"
	E_INT     Code = "E_INT"
	E_BLOCK   Code = "E_BLOCK"
	E_DIVERGE Code = "E_DIVERGE"
)

// FogError carries the location and measured values needed by diagnostics.
type FogError struct {
	Code      Code        `json:"code"`
	Message   string      `json:"message"`
	MediaID   string      `json:"media_id,omitempty"`
	Field     string      `json:"field,omitempty"`
	RayID     string      `json:"ray_id,omitempty"`
	Chunk     *ChunkCoord `json:"chunk,omitempty"`
	Actual    *float64    `json:"actual,omitempty"`
	Expected  *float64    `json:"expected,omitempty"`
	Deviation *float64    `json:"deviation,omitempty"`
	Tolerance *float64    `json:"tolerance,omitempty"`
	Cause     string      `json:"cause,omitempty"`
}

func (e *FogError) Error() string {
	if e == nil {
		return "<nil>"
	}
	loc := ""
	if e.MediaID != "" {
		loc += " media=" + e.MediaID
	}
	if e.Field != "" {
		loc += " field=" + e.Field
	}
	if e.RayID != "" {
		loc += " ray=" + e.RayID
	}
	if e.Chunk != nil {
		loc += fmt.Sprintf(" chunk=(%d,%d)", e.Chunk.X, e.Chunk.Y)
	}
	if e.Deviation != nil {
		loc += fmt.Sprintf(" deviation=%.9g", *e.Deviation)
	}
	if e.Cause != "" {
		loc += " cause=" + e.Cause
	}
	return string(e.Code) + ": " + e.Message + loc
}

func fogErr(code Code, msg string) *FogError { return &FogError{Code: code, Message: msg} }

func schemaErr(mediaID, field, msg string) *FogError {
	return &FogError{Code: E_SCHEMA, Message: msg, MediaID: mediaID, Field: field}
}

func codeErr(code Code, mediaID, field, msg string) *FogError {
	return &FogError{Code: code, Message: msg, MediaID: mediaID, Field: field}
}

func withCause(code Code, msg string, cause error) *FogError {
	e := fogErr(code, msg)
	if cause != nil {
		e.Cause = cause.Error()
	}
	return e
}

// ErrorCode returns the stable code carried by err, if any.
func ErrorCode(err error) Code {
	var fe *FogError
	if errors.As(err, &fe) {
		return fe.Code
	}
	return ""
}
