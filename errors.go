package lipsync

import (
	"errors"
	"fmt"
)

type ErrorCode string

const (
	ECSchema ErrorCode = "E_SCHEMA"
	ECOrder  ErrorCode = "E_ORDER"
	ECLabel  ErrorCode = "E_LABEL"
	ECVis    ErrorCode = "E_VIS"
	ECStage  ErrorCode = "E_STAGE"
	ECDiff   ErrorCode = "E_DIFF"
)

type Error struct {
	Code       ErrorCode `json:"code"`
	DialogueID string    `json:"dialogueId,omitempty"`
	ShardIndex int       `json:"shardIndex"`
	HasShard   bool      `json:"hasShard"`
	Frame      int       `json:"frame"`
	HasFrame   bool      `json:"hasFrame"`
	FieldPath  string    `json:"fieldPath,omitempty"`
	Message    string    `json:"message"`
}

func (e *Error) Error() string {
	location := ""
	if e.DialogueID != "" {
		location = " dialogue=" + e.DialogueID
	}
	if e.HasShard {
		location += fmt.Sprintf(" shard=%d", e.ShardIndex)
	}
	if e.HasFrame {
		location += fmt.Sprintf(" frame=%d", e.Frame)
	}
	if e.FieldPath != "" {
		location += " path=" + e.FieldPath
	}
	return fmt.Sprintf("%s%s: %s", e.Code, location, e.Message)
}

func NewError(code ErrorCode, dialogueID string, shard int, hasShard bool, path, format string, args ...any) *Error {
	return &Error{
		Code:       code,
		DialogueID: dialogueID,
		ShardIndex: shard,
		HasShard:   hasShard,
		FieldPath:  path,
		Message:    fmt.Sprintf(format, args...),
	}
}

func SchemaError(path, format string, args ...any) *Error {
	return NewError(ECSchema, "", 0, false, path, format, args...)
}

func DialogueError(code ErrorCode, d *Dialogue, path, format string, args ...any) *Error {
	shard := 0
	hasShard := false
	id := ""
	if d != nil {
		id = d.ID
		shard = d.ShardIndex
		hasShard = true
	}
	return NewError(code, id, shard, hasShard, path, format, args...)
}

func AsError(err error) (*Error, bool) {
	var domainErr *Error
	if errors.As(err, &domainErr) {
		return domainErr, true
	}
	return nil, false
}
