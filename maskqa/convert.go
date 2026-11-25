package maskqa

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"math"
)

// Truncation records one integer rounding/truncation boundary.
type Truncation struct {
	FieldPath   string `json:"field_path"`
	StrokeIndex int    `json:"stroke_index,omitempty"`
	Kind        string `json:"kind"`
	Input       int64  `json:"input"`
	Divisor     int64  `json:"divisor,omitempty"`
	Quotient    int64  `json:"quotient"`
	Remainder   int64  `json:"remainder"`
	Rounding    string `json:"rounding"`
	Position    string `json:"position"`
	Output      int64  `json:"output"`
}

func checkedMul(a, b int) (int, error) {
	ai, bi := int64(a), int64(b)
	if ai == 0 || bi == 0 {
		return 0, nil
	}
	product := ai * bi
	if product/bi != ai {
		return 0, overflowError("integer_multiply", ai, bi)
	}
	return int(product), nil
}

func checkedSub(a, b int) (int, error) {
	ai, bi := int64(a), int64(b)
	if (bi > 0 && ai < math.MinInt64+bi) || (bi < 0 && ai > math.MaxInt64+bi) {
		return 0, overflowError("integer_subtract", ai, bi)
	}
	return int(ai - bi), nil
}

func checkedAdd(a, b int) (int, error) {
	ai, bi := int64(a), int64(b)
	if (bi > 0 && ai > math.MaxInt64-bi) || (bi < 0 && ai < math.MinInt64-bi) {
		return 0, overflowError("integer_add", ai, bi)
	}
	return int(ai + bi), nil
}

// WeightPermilleToBPS converts ‰ to ‱ using exact integer multiplication.
func WeightPermilleToBPS(permille int, fieldPath string) (int, error) {
	v, err := checkedMul(permille, 10)
	if err != nil {
		qe := err.(*Error)
		qe.Field = fieldPath
		qe.Path = fieldPath
		return 0, qe
	}
	return v, nil
}

// WeightBPSToPermille converts ‱ to ‰. The ones digit is the declared truncation position.
func WeightBPSToPermille(bps int, rounding, fieldPath string, strokeIndex int) (int, Truncation, error) {
	if bps > math.MaxInt32*10 || bps < -math.MaxInt32*10 {
		return 0, Truncation{}, overflowError(fieldPath, int64(bps), 10)
	}
	q, rem := roundedDiv(bps, 10, rounding)
	tr := Truncation{
		FieldPath:   fieldPath,
		StrokeIndex: strokeIndex,
		Kind:        "weight_bps_to_permille",
		Input:       int64(bps),
		Divisor:     10,
		Quotient:    int64(q),
		Remainder:   rem,
		Rounding:    rounding,
		Position:    "weight_basis_points.ones_digit",
		Output:      int64(q),
	}
	return q, tr, nil
}

func roundedDiv(v, d int, mode string) (quotient int, remainder int64) {
	switch mode {
	case RoundingNearest:
		if v >= 0 {
			quotient = (v + d/2) / d
		} else {
			quotient = -((-v + d/2) / d)
		}
	default:
		// Integer division truncates toward zero, matching floor_to_zero.
		quotient = v / d
	}
	return quotient, int64(v - quotient*d)
}

// IndexOrigin derives the coordinate origin from a 1-based stroke number.
func IndexOrigin(index int, enc CoordinateEncoding) (Point, error) {
	if index <= 0 {
		return Point{}, paramError("stroke.index", ">=1", "stroke index must be 1-based")
	}
	n, err := checkedMul(index-1, enc.IndexStepX)
	if err != nil {
		return Point{}, err
	}
	m, err := checkedMul(index-1, enc.IndexStepY)
	if err != nil {
		return Point{}, err
	}
	return Point{X: n, Y: m}, nil
}

// AbsoluteToIndexOffset converts an absolute point to its declared index-derived offset.
func AbsoluteToIndexOffset(p Point, index int, enc CoordinateEncoding, fieldPath string) (Point, []Truncation, error) {
	origin, err := IndexOrigin(index, enc)
	if err != nil {
		if qe, ok := AsError(err); ok {
			qe.Field = fieldPath
			qe.Path = fieldPath
		}
		return Point{}, nil, err
	}
	x, err := checkedSub(p.X, origin.X)
	if err != nil {
		qe := err.(*Error)
		qe.Field = fieldPath + ".x"
		qe.Path = qe.Field
		return Point{}, nil, qe
	}
	y, err := checkedSub(p.Y, origin.Y)
	if err != nil {
		qe := err.(*Error)
		qe.Field = fieldPath + ".y"
		qe.Path = qe.Field
		return Point{}, nil, qe
	}

	trs := []Truncation{}
	q := enc.OffsetQuantum
	if q <= 1 {
		return Point{X: x, Y: y}, trs, nil
	}
	qx, rx := roundedDiv(x, q, RoundingFloor)
	qy, ry := roundedDiv(y, q, RoundingFloor)
	if rx != 0 {
		trs = append(trs, Truncation{FieldPath: fieldPath + ".x_offset", StrokeIndex: index, Kind: "coordinate_offset_quantize", Input: int64(x), Divisor: int64(q), Quotient: int64(qx), Remainder: rx, Rounding: RoundingFloor, Position: fmt.Sprintf("%s.x / offset_quantum", fieldPath), Output: int64(qx * q)})
	}
	if ry != 0 {
		trs = append(trs, Truncation{FieldPath: fieldPath + ".y_offset", StrokeIndex: index, Kind: "coordinate_offset_quantize", Input: int64(y), Divisor: int64(q), Quotient: int64(qy), Remainder: ry, Rounding: RoundingFloor, Position: fmt.Sprintf("%s.y / offset_quantum", fieldPath), Output: int64(qy * q)})
	}
	return Point{X: qx, Y: qy}, trs, nil
}

// IndexOffsetToAbsolute restores an absolute pixel coordinate. Offset values are multiplied by quantum.
func IndexOffsetToAbsolute(offset Point, index int, enc CoordinateEncoding, fieldPath string) (Point, error) {
	origin, err := IndexOrigin(index, enc)
	if err != nil {
		return Point{}, err
	}
	q := enc.OffsetQuantum
	if q <= 0 {
		q = 1
	}
	xo, err := checkedMul(offset.X, q)
	if err != nil {
		qe := err.(*Error)
		qe.Field = fieldPath + ".x"
		qe.Path = qe.Field
		return Point{}, qe
	}
	yo, err := checkedMul(offset.Y, q)
	if err != nil {
		qe := err.(*Error)
		qe.Field = fieldPath + ".y"
		qe.Path = qe.Field
		return Point{}, qe
	}
	x, err := checkedAdd(origin.X, xo)
	if err != nil {
		qe := err.(*Error)
		qe.Field = fieldPath + ".x"
		qe.Path = qe.Field
		return Point{}, qe
	}
	y, err := checkedAdd(origin.Y, yo)
	if err != nil {
		qe := err.(*Error)
		qe.Field = fieldPath + ".y"
		qe.Path = qe.Field
		return Point{}, qe
	}
	return Point{X: x, Y: y}, nil
}

// DigestResiduals returns a stable SHA-256 digest of conversion truncations.
func DigestResiduals(trs []Truncation) string {
	if len(trs) == 0 {
		sum := sha256.Sum256([]byte("no-residuals"))
		return hex.EncodeToString(sum[:])
	}
	b, _ := json.Marshal(trs)
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:])
}

// HashBytes returns the hex SHA-256 digest of b.
func HashBytes(b []byte) string {
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:])
}

// HashJSON returns a stable digest for JSON-serializable value.
func HashJSON(v any) string {
	b, err := json.Marshal(v)
	if err != nil {
		panic(err)
	}
	return HashBytes(b)
}
