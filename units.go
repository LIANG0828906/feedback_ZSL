package maskqc

import (
	"encoding/binary"
	"math"
)

const (
	AnchorCell        = 16
	WeightMaxV1       = 100
	WeightMaxV2       = 255
	WeightMaxInternal = 1000
	MinRadiusTick     = 1
	MaxRadiusTick     = 16
	MinPermille       = 0
	MaxPermille       = 1000
)

// AnchorColumns returns the declared sequence-grid column count.
func AnchorColumns(width int) int {
	cols := width / AnchorCell
	if cols < 1 {
		return 1
	}
	return cols
}

// SequenceAnchor derives an integer origin from a one-based stroke number.
// The returned remainder is index modulo column count and is retained in the
// conversion audit trail.
func SequenceAnchor(seq, width int) (Point, int) {
	if seq < 1 {
		seq = 1
	}
	index := seq - 1
	cols := AnchorColumns(width)
	col := index % cols
	row := index / cols
	return Point{X: col * AnchorCell, Y: row * AnchorCell}, index % cols
}

// AbsoluteToOffset converts an absolute point to a sequence-relative offset.
func AbsoluteToOffset(p Point, seq, width int, field string) (Point, TransformRecord, *Error) {
	anchor, rem := SequenceAnchor(seq, width)
	dx, err := checkedSub(p.X, anchor.X, field+".x", int64(p.X), int64(anchor.X))
	if err != nil {
		return Point{}, TransformRecord{}, err
	}
	dy, err := checkedSub(p.Y, anchor.Y, field+".y", int64(p.Y), int64(anchor.Y))
	if err != nil {
		return Point{}, TransformRecord{}, err
	}
	rec := TransformRecord{
		Path:        field,
		Field:       field,
		Op:          "absolute_to_seq_offset",
		Old:         p,
		New:         Point{X: dx, Y: dy},
		Rounding:    "integer_exact",
		Remainder:   int64(rem),
		TruncatedAt: "SequenceAnchor.row_div",
	}
	return Point{X: dx, Y: dy}, rec, nil
}

// OffsetToAbsolute converts a sequence-relative offset back to pixels.
func OffsetToAbsolute(offset Point, seq, width int, field string) (Point, TransformRecord, *Error) {
	anchor, rem := SequenceAnchor(seq, width)
	x, err := checkedAdd(offset.X, anchor.X, field+".x", int64(offset.X), int64(anchor.X))
	if err != nil {
		return Point{}, TransformRecord{}, err
	}
	y, err := checkedAdd(offset.Y, anchor.Y, field+".y", int64(offset.Y), int64(anchor.Y))
	if err != nil {
		return Point{}, TransformRecord{}, err
	}
	rec := TransformRecord{
		Path:        field,
		Field:       field,
		Op:          "seq_offset_to_absolute",
		Old:         offset,
		New:         Point{X: x, Y: y},
		Rounding:    "integer_exact",
		Remainder:   int64(rem),
		TruncatedAt: "SequenceAnchor.row_div",
	}
	return Point{X: x, Y: y}, rec, nil
}

// ConvertScale converts value on fromMax scale to toMax scale using
// non-negative floor division. It returns the lost numerator remainder so an
// explicit round-trip can be exact.
func ConvertScale(value, fromMax, toMax int, field string) (scaled int, remainder int64, err *Error) {
	if fromMax <= 0 || toMax <= 0 {
		return 0, 0, paramError(field, 1, math.MaxInt, "scale maxima must be positive")
	}
	if value < 0 || value > fromMax {
		return 0, 0, paramError(field, 0, fromMax, "value is outside declared scale")
	}
	v := int64(value)
	to := int64(toMax)
	from := int64(fromMax)
	if v > math.MaxInt64/to {
		return 0, 0, overflowError(field, v, to)
	}
	product := v * to
	scaled64 := product / from
	rem := product % from
	if scaled64 > math.MaxInt || scaled64 < math.MinInt {
		return 0, 0, overflowError(field, product, from)
	}
	return int(scaled64), rem, nil
}

// RestoreScale is the inverse of ConvertScale when remainder is retained.
func RestoreScale(scaled int, remainder int64, fromMax, toMax int, field string) (int, *Error) {
	if fromMax <= 0 || toMax <= 0 {
		return 0, paramError(field, 1, math.MaxInt, "scale maxima must be positive")
	}
	if scaled < 0 || scaled > toMax {
		return 0, paramError(field, 0, toMax, "scaled value is outside declared scale")
	}
	q := int64(scaled)
	from := int64(fromMax)
	to := int64(toMax)
	if q > (math.MaxInt64-remainder)/from {
		return 0, overflowError(field, q*from, remainder)
	}
	numerator := q*from + remainder
	value64 := numerator / to
	if value64 > math.MaxInt || value64 < math.MinInt {
		return 0, overflowError(field, numerator, to)
	}
	return int(value64), nil
}

// ResidualDigest summarizes an integer conversion remainder for checkpoints.
func ResidualDigest(strokeSeq int, field string, remainder int64) string {
	var buf [16]byte
	binary.BigEndian.PutUint64(buf[0:8], uint64(strokeSeq))
	binary.BigEndian.PutUint64(buf[8:16], uint64(remainder))
	return hashBytes([]byte("residual:"+field), buf[:])[:16]
}

func checkedAdd(a, b int, field string, ai, bi int64) (int, *Error) {
	sum := int64(a) + int64(b)
	if (a > 0 && b > 0 && sum < 0) || (a < 0 && b < 0 && sum > 0) || sum > math.MaxInt || sum < math.MinInt {
		return 0, overflowError(field, ai, bi)
	}
	return int(sum), nil
}

func checkedSub(a, b int, field string, ai, bi int64) (int, *Error) {
	diff := int64(a) - int64(b)
	if (a >= 0 && b < 0 && diff < 0) || (a < 0 && b > 0 && diff > 0) || diff > math.MaxInt || diff < math.MinInt {
		return 0, overflowError(field, ai, bi)
	}
	return int(diff), nil
}
