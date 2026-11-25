package diag

import (
	"errors"
	"fmt"
)

const (
	CodeSchema = "E_SCHEMA"
	CodeGeom   = "E_GEOM"
	CodeRange  = "E_RANGE"
	CodeState  = "E_STATE"
	CodeVer    = "E_VER"
	CodeDiff   = "E_DIFF"
	CodeBudget = "E_BUDGET"
	CodeFault  = "E_FAULT"
)

type Error struct {
	Code      string         `json:"code"`
	Message   string         `json:"message"`
	RoomIndex int            `json:"room_index,omitempty"`
	RoomID    string         `json:"room_id,omitempty"`
	Field     string         `json:"field,omitempty"`
	ID        string         `json:"id,omitempty"`
	Details   map[string]any `json:"details,omitempty"`
}

func (e *Error) Error() string {
	where := ""
	if e.RoomID != "" {
		where = " room=" + e.RoomID
	}
	if e.Field != "" {
		where += " field=" + e.Field
	}
	if e.ID != "" {
		where += " id=" + e.ID
	}
	return fmt.Sprintf("%s%s%s: %s", e.Code, where, suffixSpace(where), e.Message)
}

func suffixSpace(s string) string {
	if s == "" {
		return ""
	}
	return " "
}

func (e *Error) Is(target error) bool {
	var t *Error
	if errors.As(target, &t) {
		return t.Code == e.Code
	}
	return false
}

func New(code, format string, args ...any) *Error {
	return &Error{Code: code, Message: fmt.Sprintf(format, args...)}
}

func Schema(field, format string, args ...any) *Error {
	return &Error{Code: CodeSchema, Field: field, Message: fmt.Sprintf(format, args...)}
}

func Geom(roomID string, roomIndex int, field, format string, args ...any) *Error {
	return &Error{Code: CodeGeom, RoomID: roomID, RoomIndex: roomIndex, Field: field, Message: fmt.Sprintf(format, args...)}
}

func AsError(err error) (*Error, bool) {
	var de *Error
	if errors.As(err, &de) {
		return de, true
	}
	return nil, false
}

func (e *Error) WithID(id string) *Error {
	e.ID = id
	return e
}

func (e *Error) WithRoom(roomID string, roomIndex int) *Error {
	e.RoomID = roomID
	e.RoomIndex = roomIndex
	return e
}
