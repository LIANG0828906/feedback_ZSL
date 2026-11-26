package maskqc

import (
	"fmt"
	"math"
)

// CoordinateRule defines the deterministic sequence-number anchor used by
// older schemas. Anchor(seq) = (seq*StepX, seq*StepY).
type CoordinateRule struct {
	StepX int64 `json:"step_x"`
	StepY int64 `json:"step_y"`
}

var DefaultCoordinateRule = CoordinateRule{StepX: 8, StepY: 6}

// WeightDomainMyriad is the declared integer coverage-weight set: every basis
// point from 1/10000 through 10000/10000.
const (
	MinWeightMyriad = 1
	MaxWeightMyriad = 10000
	MinHardnessPct  = 0
	MaxHardnessPct  = 100
)

func CheckedMul64(a, b int64, field, objectID string) (int64, error) {
	if a == 0 || b == 0 {
		return 0, nil
	}
	if a > math.MaxInt64/b || a < math.MinInt64/b {
		return 0, &BizError{
			Code: CodeOVF, Message: "integer multiplication overflow",
			Field: field, ObjectID: objectID, A: a, B: b, ChunkID: -1,
		}
	}
	return a * b, nil
}

func CheckedAdd64(a, b int64, field, objectID string) (int64, error) {
	if (b > 0 && a > math.MaxInt64-b) || (b < 0 && a < math.MinInt64-b) {
		return 0, &BizError{
			Code: CodeOVF, Message: "integer addition overflow",
			Field: field, ObjectID: objectID, A: a, B: b, ChunkID: -1,
		}
	}
	return a + b, nil
}

func (r CoordinateRule) Anchor(seq int, objectID string) (Point, []TraceEntry, error) {
	seq64 := int64(seq)
	ax, err := CheckedMul64(seq64, r.StepX, "coordinate.anchor_x", objectID)
	if err != nil {
		return Point{}, nil, err
	}
	ay, err := CheckedMul64(seq64, r.StepY, "coordinate.anchor_y", objectID)
	if err != nil {
		return Point{}, nil, err
	}
	return Point{X: int(ax), Y: int(ay)}, []TraceEntry{{
		Rule:     "anchor=seq*step",
		Field:    "coordinate.anchor",
		NewValue: fmt.Sprintf("(%d,%d)", ax, ay),
		OperandA: seq64,
		OperandB: r.StepX*1000 + r.StepY,
	}}, nil
}

// AbsoluteToOffsets converts absolute points to anchor-relative offsets. It is
// exact; the remainder fields are retained for audit symmetry.
func AbsoluteToOffsets(start, end Point, seq int, objectID string, r CoordinateRule) (Point, Point, []TraceEntry, error) {
	anchor, anchorTrace, err := r.Anchor(seq, objectID)
	if err != nil {
		return Point{}, Point{}, nil, err
	}
	traces := anchorTrace
	conv := func(p Point, name string) (Point, error) {
		x, err := CheckedAdd64(int64(p.X), -int64(anchor.X), name+".x", objectID)
		if err != nil {
			return Point{}, err
		}
		y, err := CheckedAdd64(int64(p.Y), -int64(anchor.Y), name+".y", objectID)
		if err != nil {
			return Point{}, err
		}
		traces = append(traces, TraceEntry{
			Rule: "offset=absolute-anchor", Field: name,
			OldValue: fmt.Sprintf("(%d,%d)", p.X, p.Y), NewValue: fmt.Sprintf("(%d,%d)", x, y),
			TruncatedAt: name, Remainder: 0, OperandA: int64(p.X), OperandB: int64(anchor.X),
		})
		return Point{X: int(x), Y: int(y)}, nil
	}
	os, err := conv(start, "start_offset")
	if err != nil {
		return Point{}, Point{}, nil, err
	}
	oe, err := conv(end, "end_offset")
	if err != nil {
		return Point{}, Point{}, nil, err
	}
	return os, oe, traces, nil
}

func OffsetsToAbsolute(startOffset, endOffset Point, seq int, objectID string, r CoordinateRule) (Point, Point, []TraceEntry, error) {
	anchor, anchorTrace, err := r.Anchor(seq, objectID)
	if err != nil {
		return Point{}, Point{}, nil, err
	}
	traces := anchorTrace
	conv := func(p Point, name string) (Point, error) {
		x, err := CheckedAdd64(int64(p.X), int64(anchor.X), name+".x", objectID)
		if err != nil {
			return Point{}, err
		}
		y, err := CheckedAdd64(int64(p.Y), int64(anchor.Y), name+".y", objectID)
		if err != nil {
			return Point{}, err
		}
		traces = append(traces, TraceEntry{
			Rule: "absolute=offset+anchor", Field: name,
			OldValue: fmt.Sprintf("(%d,%d)", p.X, p.Y), NewValue: fmt.Sprintf("(%d,%d)", x, y),
			TruncatedAt: name, Remainder: 0, OperandA: int64(p.X), OperandB: int64(anchor.X),
		})
		return Point{X: int(x), Y: int(y)}, nil
	}
	s, err := conv(startOffset, "start")
	if err != nil {
		return Point{}, Point{}, nil, err
	}
	e, err := conv(endOffset, "end")
	if err != nil {
		return Point{}, Point{}, nil, err
	}
	return s, e, traces, nil
}

// MyriadToPermille converts 1/10000 to 1/1000. Rounding is "floor" as declared
// by the construction schema; remainder is w - q*10.
func MyriadToPermille(weight int) (permille int, remainder int, trace TraceEntry) {
	q := weight / 10
	rem := weight % 10
	if weight < 0 && rem != 0 {
		// Go truncates toward zero; declare floor explicitly.
		q--
		rem += 10
	}
	return q, rem, TraceEntry{
		Rule: "permille=floor(myriad/10)", Field: "weight_permille",
		OldValue: fmt.Sprint(weight), NewValue: fmt.Sprint(q),
		TruncatedAt: "weight/10", Remainder: int64(rem),
		OperandA: int64(weight), OperandB: 10,
	}
}

func PermilleToMyriad(permille int) int { return permille * 10 }

// RestoreMyriad uses a previously recorded remainder to make high->low->high
// exact. The residual ledger is persisted/exported for audit.
func RestoreMyriad(permille, remainder int) (int, error) {
	w, err := CheckedMul64(int64(permille), 10, "weight_permille", "")
	if err != nil {
		return 0, err
	}
	return int(w + int64(remainder)), nil
}

// PercentToMyriad maps v2 percentage integers to basis points.
func PercentToMyriad(percent int) int { return percent * 100 }
func MyriadToPercentFloor(weight int) (percent int, remainder int) {
	return weight / 100, weight % 100
}

func ValidateStrokeParameters(s Stroke) error {
	if s.HardnessPct < MinHardnessPct || s.HardnessPct > MaxHardnessPct {
		return &BizError{
			Code: CodePara, Message: "hardness outside declared range",
			Field: "hardness_pct", StrokeID: s.ID, ObjectID: s.ID,
			Min: MinHardnessPct, Max: MaxHardnessPct, ChunkID: -1,
			Want: fmt.Sprintf("[%d,%d]", MinHardnessPct, MaxHardnessPct),
			Got:  fmt.Sprint(s.HardnessPct),
		}
	}
	if s.WeightMyriad < MinWeightMyriad || s.WeightMyriad > MaxWeightMyriad {
		return &BizError{
			Code: CodePara, Message: "weight outside declared integer scale set",
			Field: "weight_myriad", StrokeID: s.ID, ObjectID: s.ID,
			Min: MinWeightMyriad, Max: MaxWeightMyriad, ChunkID: -1,
			Want: fmt.Sprintf("integer in [%d,%d]", MinWeightMyriad, MaxWeightMyriad),
			Got:  fmt.Sprint(s.WeightMyriad),
		}
	}
	if s.Width <= 0 {
		return &BizError{
			Code: CodePara, Message: "width must be positive",
			Field: "width", StrokeID: s.ID, ObjectID: s.ID, Min: 1, Max: math.MaxInt32,
			Got: fmt.Sprint(s.Width), ChunkID: -1,
		}
	}
	return nil
}
