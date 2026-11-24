package model

import (
	"errors"
	"fmt"
)

const (
	ErrSchema      = "E_SCHEMA"
	ErrGeometry    = "E_GEOM"
	ErrChain       = "E_CHAIN"
	ErrState       = "E_STATE"
	ErrDiff        = "E_DIFF"
	ErrResource    = "E_RESOURCE"
	ErrCheckpoint  = "E_CHECKPOINT"
	ErrIO          = "E_IO"
	ErrInterrupted = "E_INTERRUPTED"
)

type Error struct {
	Code            string `json:"code"`
	Message         string `json:"message"`
	Field           string `json:"field,omitempty"`
	RoomID          string `json:"room_id,omitempty"`
	WallID          string `json:"wall_id,omitempty"`
	DoorID          string `json:"door_id,omitempty"`
	SourceID        string `json:"source_id,omitempty"`
	ListenerID      string `json:"listener_id,omitempty"`
	FirstDivergence string `json:"first_divergence_room,omitempty"`
	Expected        any    `json:"expected,omitempty"`
	Actual          any    `json:"actual,omitempty"`
}

func (e *Error) Error() string {
	if e.Message != "" {
		return e.Code + ": " + e.Message
	}
	return e.Code
}

func NewError(code, format string, args ...any) *Error {
	return &Error{Code: code, Message: fmt.Sprintf(format, args...)}
}

func AsError(err error) (*Error, bool) {
	var target *Error
	if errors.As(err, &target) {
		return target, true
	}
	return nil, false
}

func SchemaError(field, format string, args ...any) *Error {
	e := NewError(ErrSchema, format, args...)
	e.Field = field
	return e
}

func GeometryError(roomID, wallID, field, format string, args ...any) *Error {
	e := NewError(ErrGeometry, format, args...)
	e.RoomID = roomID
	e.WallID = wallID
	e.Field = field
	return e
}
