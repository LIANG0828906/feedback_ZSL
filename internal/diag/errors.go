package diag

import (
	"encoding/json"
	"errors"
	"fmt"
	"strings"
)

const (
	ESchema    = "E_SCHEMA"
	ERef       = "E_REF"
	EPara      = "E_PARA"
	EDep       = "E_DEP"
	EDiff      = "E_DIFF"
	EDecode    = "E_DECODE"
	EIO        = "E_IO"
	EBlock     = "E_BLOCK"
	ECanceled  = "E_CANCELED"
	EBusy      = "E_BUSY"
	EExhausted = "E_EXHAUSTED"
)

// Error is the stable error contract shared by the loader, renderer, engine,
// CLI and Wails bindings. Optional locations and values are populated only
// when they are meaningful for the failure.
type Error struct {
	Code     string         `json:"code"`
	Message  string         `json:"message"`
	LightID  string         `json:"light_id,omitempty"`
	Field    string         `json:"field,omitempty"`
	CameraID string         `json:"camera_id,omitempty"`
	BlockX   int            `json:"block_x,omitempty"`
	BlockY   int            `json:"block_y,omitempty"`
	PixelX   int            `json:"pixel_x,omitempty"`
	PixelY   int            `json:"pixel_y,omitempty"`
	Item     string         `json:"item,omitempty"`
	Left     string         `json:"left,omitempty"`
	Right    string         `json:"right,omitempty"`
	Snapshot map[string]any `json:"params_snapshot,omitempty"`
	Cause    error          `json:"-"`
}

func (e *Error) Error() string {
	if e == nil {
		return ""
	}
	var b strings.Builder
	b.WriteString(e.Code)
	if e.Message != "" {
		b.WriteString(": ")
		b.WriteString(e.Message)
	}
	var loc []string
	if e.LightID != "" {
		loc = append(loc, "light="+e.LightID)
	}
	if e.Field != "" {
		loc = append(loc, "field="+e.Field)
	}
	if e.CameraID != "" {
		loc = append(loc, "camera="+e.CameraID)
	}
	if e.BlockX != 0 || e.BlockY != 0 {
		loc = append(loc, fmt.Sprintf("block=(%d,%d)", e.BlockX, e.BlockY))
	}
	if e.PixelX != 0 || e.PixelY != 0 {
		loc = append(loc, fmt.Sprintf("pixel=(%d,%d)", e.PixelX, e.PixelY))
	}
	if e.Item != "" {
		loc = append(loc, "item="+e.Item)
	}
	if len(loc) > 0 {
		b.WriteString(" [")
		b.WriteString(strings.Join(loc, ", "))
		b.WriteString("]")
	}
	if e.Cause != nil {
		b.WriteString(": ")
		b.WriteString(e.Cause.Error())
	}
	return b.String()
}

func (e *Error) Unwrap() error { return e.Cause }

func New(code, message string) *Error {
	return &Error{Code: code, Message: message}
}

func Wrap(code, message string, cause error) *Error {
	return &Error{Code: code, Message: message, Cause: cause}
}

func Schema(field, message string) *Error {
	return &Error{Code: ESchema, Field: field, Message: message}
}

func Reference(field, message string) *Error {
	return &Error{Code: ERef, Field: field, Message: message}
}

func Parameter(lightID, field, message string) *Error {
	return &Error{Code: EPara, LightID: lightID, Field: field, Message: message}
}

func Dependency(cameraID string, bx, by int, message string) *Error {
	return &Error{
		Code:     EDep,
		CameraID: cameraID,
		BlockX:   bx,
		BlockY:   by,
		Message:  message,
	}
}

func Decode(field, message string, cause error) *Error {
	return &Error{Code: EDecode, Field: field, Message: message, Cause: cause}
}

func IO(message string, cause error) *Error {
	return &Error{Code: EIO, Message: message, Cause: cause}
}

func Block(lightID, field, message string, cause error) *Error {
	return &Error{Code: EBlock, LightID: lightID, Field: field, Message: message, Cause: cause}
}

// CodeOf returns the stable diagnostic code for an arbitrary error.
func CodeOf(err error) string {
	if err == nil {
		return ""
	}
	var de *Error
	if errors.As(err, &de) {
		return de.Code
	}
	return ""
}

func As(err error) *Error {
	var de *Error
	if errors.As(err, &de) {
		return de
	}
	return nil
}

// ErrorJSON is used by the Wails boundary and persisted reports.
func ErrorJSON(err error) string {
	if err == nil {
		return ""
	}
	de := As(err)
	if de == nil {
		de = &Error{Code: EIO, Message: err.Error()}
	}
	b, marshalErr := json.Marshal(de)
	if marshalErr != nil {
		return fmt.Sprintf(`{"code":%q,"message":%q}`, de.Code, de.Error())
	}
	return string(b)
}
