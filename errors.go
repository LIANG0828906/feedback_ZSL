package terrainbake

import (
	"encoding/json"
	"errors"
	"fmt"
	"strings"
)

// Code is part of the public diagnostic contract. The HTTP API, loader and
// baking paths all return these values verbatim.
type Code string

const (
	CodeSchema Code = "E_SCHEMA"
	CodeOvf    Code = "E_OVF"
	CodePara   Code = "E_PARA"
	CodeAttr   Code = "E_ATTR"
	CodeDiff   Code = "E_DIFF"
	CodeDecode Code = "E_DECODE"
	CodeOver   Code = "E_OVER"
	CodeAdj    Code = "E_ADJ"
	CodeBlock  Code = "E_BLOCK"
)

// Error carries stable machine-readable context in addition to a human
// message. Fields that do not apply to a particular code are zero-valued.
type Error struct {
	Code       Code   `json:"code"`
	Path       string `json:"path,omitempty"`
	Field      string `json:"field,omitempty"`
	Message    string `json:"message"`
	OperandA   string `json:"operand_a,omitempty"`
	OperandB   string `json:"operand_b,omitempty"`
	Legal      string `json:"legal,omitempty"`
	Boundary   string `json:"boundary,omitempty"`
	BlockA     string `json:"block_a,omitempty"`
	BlockB     string `json:"block_b,omitempty"`
	CountA     int    `json:"count_a,omitempty"`
	CountB     int    `json:"count_b,omitempty"`
	BlockID    string `json:"block_id,omitempty"`
	SideA      string `json:"side_a,omitempty"`
	SideB      string `json:"side_b,omitempty"`
	Mismatch   string `json:"mismatch_field,omitempty"`
	LHS        string `json:"lhs,omitempty"`
	RHS        string `json:"rhs,omitempty"`
	Residual   int64  `json:"residual,omitempty"`
	Truncation string `json:"truncation,omitempty"`
	Detail     string `json:"detail,omitempty"`
}

func (e *Error) Error() string {
	if e == nil {
		return ""
	}
	var b strings.Builder
	b.WriteString(string(e.Code))
	if e.Path != "" {
		b.WriteString(" path=")
		b.WriteString(e.Path)
	}
	if e.Field != "" {
		b.WriteString(" field=")
		b.WriteString(e.Field)
	}
	if e.BlockID != "" {
		b.WriteString(" block=")
		b.WriteString(e.BlockID)
	}
	if e.Boundary != "" {
		b.WriteString(" boundary=")
		b.WriteString(e.Boundary)
	}
	if e.Message != "" {
		b.WriteString(": ")
		b.WriteString(e.Message)
	}
	return b.String()
}

// MarshalJSON keeps the concrete code visible to clients that only inspect
// the JSON payload.
func (e Error) MarshalJSON() ([]byte, error) {
	type alias Error
	return json.Marshal(alias(e))
}

func IsCode(err error, code Code) bool {
	var target *Error
	return errors.As(err, &target) && target.Code == code
}

func errSchema(path, message string) *Error {
	return &Error{Code: CodeSchema, Path: path, Field: path, Message: message}
}

func errDecode(path, message string) *Error {
	return &Error{Code: CodeDecode, Path: path, Field: path, Message: message}
}

func errPara(field, legal, message string) *Error {
	return &Error{Code: CodePara, Field: field, Path: field, Legal: legal, Message: message}
}

func errOvf(field string, a, b int64) *Error {
	return &Error{
		Code:     CodeOvf,
		Field:    field,
		Path:     field,
		OperandA: fmt.Sprintf("%d", a),
		OperandB: fmt.Sprintf("%d", b),
		Message:  "integer conversion overflow",
	}
}

func errAdj(path, field, message string) *Error {
	return &Error{Code: CodeAdj, Path: path, Field: field, Message: message}
}

func errOver(path string, a, b string, message string) *Error {
	return &Error{
		Code:    CodeOver,
		Path:    path,
		Field:   path,
		BlockA:  a,
		BlockB:  b,
		Message: message,
	}
}
