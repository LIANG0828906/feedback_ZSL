package pipeline

import (
	"fmt"
	"strings"
)

const (
	CodeVersion   = "E_VER"
	CodeSchema    = "E_SCHEMA"
	CodeDecode    = "E_DECODE"
	CodeParameter = "E_PARA"
	CodeLayer     = "E_LAYER"
	CodeState     = "E_STATE"
	CodeLossy     = "E_LOSSY"
)

// Error is the stable error envelope used by both the Go API and the CLI.
// Path and Field are populated for schema errors, LayerID for layer errors and
// Window for state errors so callers can locate the failing object.
type Error struct {
	Code    string `json:"code"`
	Message string `json:"message"`
	Path    string `json:"path,omitempty"`
	Field   string `json:"field,omitempty"`
	LayerID string `json:"layer_id,omitempty"`
	Window  int    `json:"window,omitempty"`
}

func (e *Error) Error() string {
	var details []string
	if e.Path != "" {
		details = append(details, "path="+e.Path)
	}
	if e.Field != "" {
		details = append(details, "field="+e.Field)
	}
	if e.LayerID != "" {
		details = append(details, "layer="+e.LayerID)
	}
	if e.Window >= 0 {
		details = append(details, fmt.Sprintf("window=%d", e.Window))
	}
	suffix := ""
	if len(details) > 0 {
		suffix = " (" + strings.Join(details, ", ") + ")"
	}
	return e.Code + ": " + e.Message + suffix
}

func errCode(code, format string, args ...any) *Error {
	return &Error{Code: code, Message: fmt.Sprintf(format, args...), Window: -1}
}

func schemaError(path, field, format string, args ...any) *Error {
	e := errCode(CodeSchema, format, args...)
	e.Path = path
	e.Field = field
	return e
}

func versionError(path, field, format string, args ...any) *Error {
	e := errCode(CodeVersion, format, args...)
	e.Path = path
	e.Field = field
	return e
}

func layerError(id, format string, args ...any) *Error {
	e := errCode(CodeLayer, format, args...)
	e.LayerID = id
	return e
}

func stateError(window int, format string, args ...any) *Error {
	e := errCode(CodeState, format, args...)
	e.Window = window
	return e
}

func paraError(window int, format string, args ...any) *Error {
	e := errCode(CodeParameter, format, args...)
	e.Window = window
	return e
}

// LossyWarning is emitted whenever an export must flatten or normalize a
// property that the target format cannot represent.
type LossyWarning struct {
	Code      string `json:"code"`
	LayerID   string `json:"layer_id,omitempty"`
	Property  string `json:"property"`
	From      string `json:"from"`
	To        string `json:"to"`
	Message   string `json:"message"`
	TracePath string `json:"trace_path,omitempty"`
}

func (w LossyWarning) Error() string {
	return fmt.Sprintf("%s: %s %s -> %s: %s", w.Code, w.Property, w.From, w.To, w.Message)
}
