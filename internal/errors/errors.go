// Package errors defines the stable error codes used across the pipeline.
package errors

import "fmt"

// Stable error codes required by the task specification.
const (
	ESchema = "E_SCHEMA"
	EGeom   = "E_GEOM"
	EObs    = "E_OBS"
	EZone   = "E_ZONE"
	EDep    = "E_DEP"
	EDiff   = "E_DIFF"
	EBound  = "E_BOUND"
	EVer    = "E_VER"
)

// PipelineError carries a stable code plus structured diagnostic context.
type PipelineError struct {
	Code    string
	Message string
	Path    string
	Context map[string]any
}

func (e *PipelineError) Error() string {
	s := e.Code + ": " + e.Message
	if e.Path != "" {
		s += " (path=" + e.Path + ")"
	}
	for k, v := range e.Context {
		s += fmt.Sprintf(" %s=%v", k, v)
	}
	return s
}

// New builds a coded error with a free-form message.
func New(code, format string, args ...any) error {
	return &PipelineError{Code: code, Message: fmt.Sprintf(format, args...)}
}

// WithPath attaches a field path (used for E_SCHEMA diagnostics).
func WithPath(code, path, format string, args ...any) error {
	return &PipelineError{Code: code, Path: path, Message: fmt.Sprintf(format, args...)}
}

// WithContext attaches structured context such as zone and edge ids.
func WithContext(code string, ctx map[string]any, format string, args ...any) error {
	return &PipelineError{Code: code, Context: ctx, Message: fmt.Sprintf(format, args...)}
}

// Code extracts the stable code from an error, returning "" for plain errors.
func Code(err error) string {
	if pe, ok := err.(*PipelineError); ok {
		return pe.Code
	}
	return ""
}
