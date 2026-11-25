package maskqc

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"math"
)

type RoundingMode string

const (
	RoundFloor   RoundingMode = "floor"
	RoundCeil    RoundingMode = "ceil"
	RoundNearest RoundingMode = "nearest"
)

type Truncation struct {
	Field     string       `json:"field"`
	Position  string       `json:"position"`
	Kind      string       `json:"kind"`
	Input     int64        `json:"input"`
	Output    int64        `json:"output"`
	Remainder int64        `json:"remainder"`
	Operands  []int64      `json:"operands"`
	Rounding  RoundingMode `json:"rounding"`
	Truncated bool         `json:"truncated"`
}

type ConversionReport struct {
	Entries []Truncation `json:"entries"`
}

func (r *ConversionReport) record(t Truncation) {
	if r != nil {
		r.Entries = append(r.Entries, t)
	}
}

func (r *ConversionReport) ResidualDigest() string {
	if r == nil {
		return sha256Sum(nil)
	}
	b, _ := json.Marshal(r.Entries)
	return sha256Sum(b)
}

func sha256Sum(b []byte) string {
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:])
}

// AnchorForSeq is the deterministic origin used by the v2 index-offset form.
// It deliberately varies in both axes so a loader cannot confuse dx/dy order.
func AnchorForSeq(seq int) Point {
	return Point{X: 3 * seq, Y: 2 * seq}
}

func checkedAddInt64(a, b int64, field string) (int64, error) {
	if (b > 0 && a > math.MaxInt64-b) || (b < 0 && a < math.MinInt64-b) {
		return 0, overflowError(field, a, b)
	}
	return a + b, nil
}

func OffsetFromAbsolute(p Point, seq int, field string, report *ConversionReport) (Point, error) {
	anchorX, anchorY, err := checkedAnchor(seq, field)
	if err != nil {
		return Point{}, err
	}
	x, err := checkedAddInt64(int64(p.X), -anchorX, field+".x")
	if err != nil {
		return Point{}, err
	}
	y, err := checkedAddInt64(int64(p.Y), -anchorY, field+".y")
	if err != nil {
		return Point{}, err
	}
	out := Point{X: int(x), Y: int(y)}
	report.record(Truncation{
		Field: field, Position: field, Kind: "absolute_to_index_offset",
		Input: int64(p.X*100000 + p.Y), Output: int64(out.X*100000 + out.Y),
		Operands: []int64{int64(p.X), int64(seq)}, Rounding: RoundFloor,
	})
	return out, nil
}

func AbsoluteFromOffset(offset Point, seq int, field string, report *ConversionReport) (Point, error) {
	anchorX, anchorY, err := checkedAnchor(seq, field)
	if err != nil {
		return Point{}, err
	}
	x, err := checkedAddInt64(int64(offset.X), anchorX, field+".x")
	if err != nil {
		return Point{}, err
	}
	y, err := checkedAddInt64(int64(offset.Y), anchorY, field+".y")
	if err != nil {
		return Point{}, err
	}
	out := Point{X: int(x), Y: int(y)}
	report.record(Truncation{
		Field: field, Position: field, Kind: "index_offset_to_absolute",
		Input: int64(offset.X*100000 + offset.Y), Output: int64(out.X*100000 + out.Y),
		Operands: []int64{int64(offset.X), int64(seq)}, Rounding: RoundFloor,
	})
	return out, nil
}

func checkedAnchor(seq int, field string) (int64, int64, error) {
	anchorX, err := checkedMul(int64(seq), 3, field+".anchorX")
	if err != nil {
		return 0, 0, err
	}
	anchorY, err := checkedMul(int64(seq), 2, field+".anchorY")
	if err != nil {
		return 0, 0, err
	}
	return anchorX, anchorY, nil
}

// ConvertScale performs basis conversion entirely with integer arithmetic.
// The returned remainder is the discarded numerator residue after division.
func ConvertScale(value int64, fromBasis, toBasis int, rounding RoundingMode, field, position string, report *ConversionReport) (int, Truncation, error) {
	if fromBasis <= 0 || toBasis <= 0 {
		return 0, Truncation{}, parameterError(field, 1, math.MaxInt32, "scale basis must be positive")
	}
	scaled, err := checkedMul(value, int64(toBasis), field)
	if err != nil {
		return 0, Truncation{}, err
	}
	q := scaled / int64(fromBasis)
	rem := scaled % int64(fromBasis)
	if rem < 0 {
		rem = -rem
	}
	originalQ := q
	truncated := rem != 0
	switch rounding {
	case RoundFloor:
		if scaled < 0 && rem != 0 {
			q--
		}
	case RoundCeil:
		if scaled > 0 && rem != 0 {
			q++
		} else if scaled < 0 && rem != 0 {
			q++ // truncation toward zero plus one moves toward +infinity for negatives? Fix below.
		}
	case RoundNearest:
		if rem*2 >= int64(fromBasis) {
			if scaled >= 0 {
				q++
			} else {
				q--
			}
		}
	}
	if rounding == RoundCeil && scaled < 0 && rem != 0 {
		// Go quotient truncated toward zero already equals ceil for negatives.
		q = originalQ
	}
	if q > math.MaxInt32 || q < math.MinInt32 {
		return 0, Truncation{}, overflowError(field, value, int64(toBasis))
	}
	t := Truncation{
		Field: field, Position: position, Kind: "scale",
		Input: value, Output: q, Remainder: rem,
		Operands: []int64{value, int64(fromBasis), int64(toBasis)},
		Rounding: rounding, Truncated: truncated || q != originalQ,
	}
	report.record(t)
	return int(q), t, nil
}

func checkedMul(a, b int64, field string) (int64, error) {
	if a == 0 || b == 0 {
		return 0, nil
	}
	if a > 0 && b > 0 && a > math.MaxInt64/b {
		return 0, overflowError(field, a, b)
	}
	if a < 0 && b < 0 && a < math.MaxInt64/b {
		return 0, overflowError(field, a, b)
	}
	if a > 0 && b < 0 && b < math.MinInt64/a {
		return 0, overflowError(field, a, b)
	}
	if a < 0 && b > 0 && a < math.MinInt64/b {
		return 0, overflowError(field, a, b)
	}
	return a * b, nil
}

func ContainsInt(list []int, v int) bool {
	for _, candidate := range list {
		if candidate == v {
			return true
		}
	}
	return false
}
