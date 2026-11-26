package maskqc

import (
	"fmt"
	"math"
)

type RoundingDirection string

const (
	RoundFloor  RoundingDirection = "declared-floor"
	RoundCeil   RoundingDirection = "declared-ceil"
	RoundHalfUp RoundingDirection = "declared-half-up"
)

// FieldTrace records one deterministic field conversion, including integer remainder.
type FieldTrace struct {
	IconID    string            `json:"icon_id,omitempty"`
	StrokeID  string            `json:"stroke_id,omitempty"`
	FieldPath string            `json:"field_path"`
	Operation string            `json:"operation"`
	OldValue  string            `json:"old_value"`
	NewValue  string            `json:"new_value"`
	Source    string            `json:"source,omitempty"`
	Rounding  RoundingDirection `json:"rounding,omitempty"`
	Truncated bool              `json:"truncated"`
	Residual  int64             `json:"residual,omitempty"`
	OperandA  int64             `json:"operand_a,omitempty"`
	OperandB  int64             `json:"operand_b,omitempty"`
}

type LossyItem struct {
	Code     ErrorCode `json:"code"`
	StrokeID string    `json:"stroke_id,omitempty"`
	Path     string    `json:"path"`
	Feature  string    `json:"feature"`
	Action   string    `json:"action"`
	Detail   string    `json:"detail"`
}

type ConvertReport struct {
	Code       ErrorCode    `json:"code,omitempty"`
	FromSchema string       `json:"from_schema"`
	ToSchema   string       `json:"to_schema"`
	Traces     []FieldTrace `json:"traces"`
	Lossy      []LossyItem  `json:"lossy,omitempty"`
}

func (r *ConvertReport) trace(t FieldTrace) {
	r.Traces = append(r.Traces, t)
}

// ScalePermyriadToPermille converts 0..10000 to 0..1000. The declared policy is
// floor; residual is the discarded units on the source scale.
func ScalePermyriadToPermille(v int, path string) (int, int, FieldTrace, error) {
	if v < 0 || v > 10000 {
		return 0, 0, FieldTrace{}, paraError(path, v, 0, 10000)
	}
	q := v / 10
	rem := v % 10
	tr := FieldTrace{
		FieldPath: path, Operation: "weight_permyriad_to_permille_div10",
		OldValue: fmt.Sprint(v), NewValue: fmt.Sprint(q), Rounding: RoundFloor,
		Truncated: rem != 0, Residual: int64(rem), OperandA: int64(v), OperandB: 10,
	}
	return q, rem, tr, nil
}

func ScalePermilleToPermyriad(v int, path string) (int, FieldTrace, error) {
	if v < 0 || v > 1000 {
		return 0, FieldTrace{}, paraError(path, v, 0, 1000)
	}
	if v > math.MaxInt32/10 {
		return 0, FieldTrace{}, ovfError(path, int64(v), 10)
	}
	out := v * 10
	tr := FieldTrace{
		FieldPath: path, Operation: "weight_permille_to_permyriad_mul10",
		OldValue: fmt.Sprint(v), NewValue: fmt.Sprint(out),
		OperandA: int64(v), OperandB: 10,
	}
	return out, tr, nil
}

// SequenceOffset stores an absolute coordinate as a pen-sequence-relative
// offset. Anchor = seq*stride; the remaining pixel coordinate is stored directly.
// The division helper documents declared floor division and its remainder.
func AbsoluteToSequenceOffset(abs, seq, stride int, path string) (offset, anchor int, tr FieldTrace, err error) {
	if seq < 0 || stride <= 0 {
		return 0, 0, FieldTrace{}, paraError(path, seq, 0, math.MaxInt32)
	}
	if seq > 0 && (stride > math.MaxInt32/seq || seq*stride > math.MaxInt32) {
		return 0, 0, FieldTrace{}, ovfError(path, int64(seq), int64(stride))
	}
	anchor = seq * stride
	offset = abs - anchor
	if abs > 0 && anchor > math.MaxInt32-abs {
		return 0, 0, FieldTrace{}, ovfError(path+".offset", int64(abs), int64(anchor))
	}
	// Block quotient is recorded for diagnostics even though the schema stores
	// the exact remainder-bearing offset. This makes truncation points explicit.
	quot := offset / stride
	rem := offset % stride
	if rem < 0 { // Go remainder follows numerator; declared floor keeps positive residue.
		quot--
		rem += stride
	}
	tr = FieldTrace{
		FieldPath: path, Operation: "absolute_to_seq_relative_floor_div",
		OldValue: fmt.Sprint(abs), NewValue: fmt.Sprintf("offset=%d,block=%d", offset, quot),
		Rounding: RoundFloor, Truncated: rem != 0, Residual: int64(rem),
		OperandA: int64(offset), OperandB: int64(stride),
	}
	return offset, anchor, tr, nil
}

func SequenceOffsetToAbsolute(offset, seq, stride int, path string) (int, FieldTrace, error) {
	if seq < 0 || stride <= 0 || seq > math.MaxInt32/stride {
		return 0, FieldTrace{}, ovfError(path, int64(seq), int64(stride))
	}
	anchor := seq * stride
	if (offset > 0 && anchor > math.MaxInt32-offset) || (offset < 0 && anchor < math.MinInt32-offset) {
		return 0, FieldTrace{}, ovfError(path, int64(offset), int64(anchor))
	}
	abs := offset + anchor
	tr := FieldTrace{
		FieldPath: path, Operation: "seq_relative_to_absolute_add_anchor",
		OldValue: fmt.Sprintf("offset=%d", offset), NewValue: fmt.Sprint(abs),
		OperandA: int64(offset), OperandB: int64(anchor),
	}
	return abs, tr, nil
}

func checkedMulInt32(a, b int, path string) (int, error) {
	if a == 0 || b == 0 {
		return 0, nil
	}
	r := int64(a) * int64(b)
	if r < math.MinInt32 || r > math.MaxInt32 {
		return 0, ovfError(path, int64(a), int64(b))
	}
	return int(r), nil
}
