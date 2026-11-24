package cad

import (
	"errors"
	"fmt"
)

const (
	CodeVersion = "E_VER"
	CodeLossy   = "E_LOSSY"
	CodeParam   = "E_PARA"
	CodeKnot    = "E_KNOT"
	CodeClip    = "E_CLIP"
	CodeDiff    = "E_DIFF"
)

type Error struct {
	Code    string         `json:"code"`
	Message string         `json:"message"`
	Version string         `json:"version,omitempty"`
	Path    string         `json:"path,omitempty"`
	PatchID string         `json:"patch_id,omitempty"`
	Point   *Vec2          `json:"conflict_point,omitempty"`
	TileID  int            `json:"tile_id,omitempty"`
	Pixel   *[2]int        `json:"pixel,omitempty"`
	Field   string         `json:"field,omitempty"`
	Details map[string]any `json:"details,omitempty"`
}

func (e *Error) Error() string {
	msg := fmt.Sprintf("%s: %s", e.Code, e.Message)
	if e.Version != "" {
		msg += " [version=" + e.Version + "]"
	}
	if e.Path != "" {
		msg += " [path=" + e.Path + "]"
	}
	if e.PatchID != "" {
		msg += " [patch=" + e.PatchID + "]"
	}
	if e.TileID != 0 || e.Pixel != nil {
		msg += fmt.Sprintf(" [tile=%d", e.TileID)
		if e.Pixel != nil {
			msg += fmt.Sprintf(",pixel=(%d,%d)", e.Pixel[0], e.Pixel[1])
		}
		msg += "]"
	}
	if e.Field != "" {
		msg += " [field=" + e.Field + "]"
	}
	return msg
}

func NewError(code, format string, args ...any) *Error {
	return &Error{Code: code, Message: fmt.Sprintf(format, args...)}
}

func (e *Error) WithVersion(version string) *Error {
	e.Version = version
	return e
}

func (e *Error) WithPath(path string) *Error {
	e.Path = path
	return e
}

func (e *Error) WithPatch(id string) *Error {
	e.PatchID = id
	return e
}

func (e *Error) WithPoint(p Vec2) *Error {
	e.Point = &p
	return e
}

func (e *Error) WithTilePixel(tile int, x, y int) *Error {
	e.TileID = tile
	e.Pixel = &[2]int{x, y}
	return e
}

func (e *Error) WithField(field string) *Error {
	e.Field = field
	return e
}

func AsError(err error) (*Error, bool) {
	var ce *Error
	if errors.As(err, &ce) {
		return ce, true
	}
	return nil, false
}
