package maskqa

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"math"
	"strings"
)

const (
	ConversionVersion = "conv-int-v1"
	OrderVersion      = "priority-seq-v1"
	AlgorithmVersion  = "raster-coverage-int-v1"

	// WeightScalePermille is the v1/v2 style scale.
	WeightScalePermille int64 = 1000
	// WeightScalePermyriad is the internal/v3 scale.
	WeightScalePermyriad int64 = 10000

	// WeightRounding declares how low-scale remainders are discarded.
	WeightRounding = "floor_non_negative"
)

// TruncationRecord is an integer conversion audit entry.
type TruncationRecord struct {
	Field     string `json:"field"`
	Operation string `json:"operation"`
	Position  string `json:"position"`
	OperandA  int64  `json:"operand_a"`
	OperandB  int64  `json:"operand_b"`
	Quotient  int64  `json:"quotient,omitempty"`
	Remainder int64  `json:"remainder"`
	Rounding  string `json:"rounding,omitempty"`
	Result    int64  `json:"result"`
	Truncated bool   `json:"truncated"`
}

func checkedAdd(a, b int64) (int64, bool) {
	if b > 0 && a > math.MaxInt64-b {
		return 0, false
	}
	if b < 0 && a < math.MinInt64-b {
		return 0, false
	}
	return a + b, true
}

func checkedSub(a, b int64) (int64, bool) {
	if b > 0 && a < math.MinInt64+b {
		return 0, false
	}
	if b < 0 && a > math.MaxInt64+b {
		return 0, false
	}
	return a - b, true
}

func checkedMul(a, b int64) (int64, bool) {
	if a == 0 || b == 0 {
		return 0, true
	}
	result := a * b
	if result/a != b {
		return 0, false
	}
	return result, true
}

// ScalePermilleToPermyriad converts 0..1000 weights to 0..10000 exactly.
func ScalePermilleToPermyriad(value int64, field string, records *[]TruncationRecord) (int64, error) {
	result, ok := checkedMul(value, 10)
	if !ok {
		return 0, ovfError(field, value, 10)
	}
	if records != nil {
		*records = append(*records, TruncationRecord{
			Field: field, Operation: "permille_to_permyriad_mul10", Position: field,
			OperandA: value, OperandB: 10, Result: result, Rounding: "exact",
		})
	}
	return result, nil
}

// ScalePermyriadToPermille floors a permyriad value and returns q and remainder.
func ScalePermyriadToPermille(value int64, field, position string, records *[]TruncationRecord) (quotient, remainder int64, err error) {
	if value < 0 {
		return 0, 0, paraError(field, 0, WeightScalePermyriad, "weight must be non-negative for declared floor rounding")
	}
	quotient = value / 10
	remainder = value % 10
	if records != nil {
		*records = append(*records, TruncationRecord{
			Field: field, Operation: "permyriad_to_permille_div10_floor", Position: position,
			OperandA: value, OperandB: 10, Quotient: quotient, Remainder: remainder,
			Rounding: WeightRounding, Result: quotient, Truncated: remainder != 0,
		})
	}
	return quotient, remainder, nil
}

// OffsetFromAbsolute performs checked absolute -> sequence-origin offset.
func OffsetFromAbsolute(absolute, origin int64, field string) (int64, error) {
	result, ok := checkedSub(absolute, origin)
	if !ok {
		return 0, ovfError(field, absolute, origin)
	}
	return result, nil
}

// AbsoluteFromOffset performs checked sequence-origin offset -> absolute.
func AbsoluteFromOffset(offset, origin int64, field string) (int64, error) {
	result, ok := checkedAdd(offset, origin)
	if !ok {
		return 0, ovfError(field, offset, origin)
	}
	return result, nil
}

// SequenceOrigin is the declared integer origin used by offset schemas.
// It is deliberately a pure function of stroke sequence and canvas size.
func SequenceOrigin(seq, axis, dimension int) (int, error) {
	if dimension <= 0 {
		return 0, paraError("canvas", 1, math.MaxInt32, "dimension must be positive")
	}
	coefficient := int64(37)
	constant := int64(11)
	if axis == 1 {
		coefficient = 19
		constant = 7
	}
	product, ok := checkedMul(int64(seq), coefficient)
	if !ok {
		return 0, ovfError(fmt.Sprintf("strokes[%d].offset_origin", seq), int64(seq), coefficient)
	}
	sum, ok := checkedAdd(product, constant)
	if !ok {
		return 0, ovfError(fmt.Sprintf("strokes[%d].offset_origin", seq), product, constant)
	}
	return int(modPositive(sum, int64(dimension))), nil
}

func modPositive(a, b int64) int64 {
	r := a % b
	if r < 0 {
		r += b
	}
	return r
}

// ResidualDigest hashes all conversion remainders in a stable order.
func ResidualDigest(records []TruncationRecord) string {
	h := sha256.New()
	for _, r := range records {
		fmt.Fprintf(h, "%s|%s|%d|%d|%d|%d\n", r.Field, r.Operation, r.OperandA, r.OperandB, r.Quotient, r.Remainder)
	}
	return hex.EncodeToString(h.Sum(nil))
}

// ContentDigest returns a short SHA-256 content hash.
func ContentDigest(parts ...[]byte) string {
	h := sha256.New()
	for _, part := range parts {
		var length [8]byte
		v := uint64(len(part))
		for i := 7; i >= 0; i-- {
			length[i] = byte(v & 0xff)
			v >>= 8
		}
		h.Write(length[:])
		h.Write(part)
	}
	return hex.EncodeToString(h.Sum(nil))
}

// StableStringDigest hashes canonical text.
func StableStringDigest(lines ...string) string {
	return ContentDigest([]byte(strings.Join(lines, "\n")))
}
