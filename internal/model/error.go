package model

import (
	"encoding/json"
	"fmt"
)

type Diff struct {
	EventSeq int         `json:"event_seq"`
	Field    string      `json:"field"`
	Expected interface{} `json:"expected,omitempty"`
	Actual   interface{} `json:"actual,omitempty"`
	Stream   string      `json:"stream,omitempty"`
}

type Error struct {
	Code      string      `json:"code"`
	Field     string      `json:"field,omitempty"`
	Item      interface{} `json:"item,omitempty"`
	Measured  interface{} `json:"measured,omitempty"`
	Detail    string      `json:"detail,omitempty"`
	FirstDiff *Diff       `json:"first_diff,omitempty"`
}

func (e *Error) Error() string {
	b, _ := json.Marshal(e)
	return string(b)
}

func Schema(field string, item interface{}, detail string) *Error {
	if text, ok := item.(string); ok && text == "" {
		item = nil
	}
	return &Error{Code: CodeSchema, Field: field, Item: item, Detail: detail}
}

func Param(field string, measured interface{}, detail string) *Error {
	return &Error{Code: CodeParam, Field: field, Measured: measured, Detail: detail}
}

func DiffError(d *Diff) *Error {
	return &Error{Code: CodeDiff, Field: d.Field, Item: fmt.Sprintf("event:%d", d.EventSeq), FirstDiff: d, Detail: "incremental and full replay differ"}
}

func Version(detail string, measured interface{}) *Error {
	return &Error{Code: CodeVer, Detail: detail, Measured: measured}
}
