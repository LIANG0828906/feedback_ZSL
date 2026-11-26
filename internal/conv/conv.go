// Package conv contains the integer-only conversion kernel used for scale,
// exposure and gain conversions. It never uses floating point for accumulated
// values.
package conv

import (
	"math"
	"math/bits"

	"msworkbench/internal/e"
)

const (
	PermilleScale   int64 = 1_000
	PermyriadScale  int64 = 10_000
	MicroPerMilli   int64 = 1_000
	GainPPMUnity    int64 = 1_000_000
	MinLegalGainPPM int64 = 500_000
	MaxLegalGainPPM int64 = 2_000_000
)

// Truncation records one declared floor/truncation operation and the location
// at which a remainder was discarded.
type Truncation struct {
	Field            string `json:"field"`
	FromVersion      string `json:"from_version,omitempty"`
	ToVersion        string `json:"to_version,omitempty"`
	Operation        string `json:"operation"`
	Numerator        int64  `json:"numerator"`
	Denominator      int64  `json:"denominator"`
	Quotient         int64  `json:"quotient"`
	Remainder        int64  `json:"remainder"`
	Direction        string `json:"direction"`
	DigitPosition    int    `json:"digit_position"`
	ResidualSequence int    `json:"residual_sequence"`
}

// Recorder collects deterministic conversion residues.
type Recorder struct {
	Truncations []Truncation `json:"truncations"`
}

func (r *Recorder) record(t Truncation) Truncation {
	t.Direction = "floor_nonnegative"
	t.DigitPosition = DecimalDigits(t.Denominator) - 1
	t.ResidualSequence = len(r.Truncations)
	r.Truncations = append(r.Truncations, t)
	return t
}

func DecimalDigits(v int64) int {
	if v <= 1 {
		return 0
	}
	n := 0
	for v > 1 {
		v /= 10
		n++
	}
	return n
}

// CheckedMul returns E_OVF with the field and the two immediate operands.
func CheckedMul(a, b int64, field string) (int64, error) {
	if a == 0 || b == 0 {
		return 0, nil
	}
	if a > math.MaxInt64/b || a < math.MinInt64/b {
		return 0, e.Overflow(field, a, b)
	}
	return a * b, nil
}

// MulDivFloor computes floor(a*b/den) using 128-bit intermediate product.
// The returned remainder is the residue after floor division.
func MulDivFloor(a, b, den int64, field string, rec *Recorder, op string) (int64, int64, error) {
	if den <= 0 {
		return 0, 0, e.Param(field, ">0", "division denominator must be positive")
	}
	if a < 0 || b < 0 {
		return 0, 0, e.Param(field, ">=0", "conversion operands must be non-negative")
	}
	hi, lo := bits.Mul64(uint64(a), uint64(b))
	if hi >= uint64(den) {
		return 0, 0, e.Overflow(field, a, b)
	}
	q64, rem := bits.Div64(hi, lo, uint64(den))
	if q64 > math.MaxInt64 {
		return 0, 0, e.Overflow(field, a, b)
	}
	q := int64(q64)
	if rec != nil && rem != 0 {
		rec.record(Truncation{
			Field:       field,
			Operation:   op,
			Numerator:   a,
			Denominator: den,
			Quotient:    q,
			Remainder:   int64(rem),
		})
	}
	return q, int64(rem), nil
}

// RescaleFloor converts v from one positive integer scale to another.
func RescaleFloor(v, fromScale, toScale int64, field string, rec *Recorder) (int64, int64, error) {
	if fromScale <= 0 || toScale <= 0 {
		return 0, 0, e.Param(field, "scales > 0", "invalid integer scale")
	}
	if v < 0 {
		return 0, 0, e.Param(field, ">=0", "scaled value must be non-negative")
	}
	if fromScale == toScale {
		return v, 0, nil
	}
	if toScale > fromScale && toScale%fromScale == 0 {
		factor := toScale / fromScale
		out, err := CheckedMul(v, factor, field)
		return out, 0, err
	}
	if fromScale > toScale && fromScale%toScale == 0 {
		factor := fromScale / toScale
		q := v / factor
		rem := v % factor
		if rec != nil && rem != 0 {
			rec.record(Truncation{
				Field: field, Operation: "rescale_floor",
				Numerator: v, Denominator: factor, Quotient: q, Remainder: rem,
			})
		}
		return q, rem, nil
	}
	return MulDivFloor(v, toScale, fromScale, field, rec, "rescale_floor")
}

func PermilleToPermyriad(v int64, rec *Recorder) (int64, error) {
	out, _, err := RescaleFloor(v, PermilleScale, PermyriadScale, "quantization.scale", rec)
	return out, err
}

func PermyriadToPermille(v int64, rec *Recorder) (int64, int64, error) {
	return RescaleFloor(v, PermyriadScale, PermilleScale, "quantization.scale", rec)
}

func MicroToMilli(us int64, rec *Recorder) (int64, int64, error) {
	return RescaleFloor(us, MicroPerMilli, 1, "exposure.stack[].value", rec)
}

func MilliToMicro(ms int64) (int64, error) {
	return CheckedMul(ms, MicroPerMilli, "exposure.stack[].value_ms")
}

func PermilleGainToPPM(gainPermille *int64) (int64, error) {
	if gainPermille == nil {
		return GainPPMUnity, nil
	}
	return CheckedMul(*gainPermille, 1000, "bands[].gain_correction_permille")
}

func PPMGainToPermille(gainPPM int64, rec *Recorder) (int64, int64, error) {
	return RescaleFloor(gainPPM, 1000, 1, "bands[].gain_correction_ppm", rec)
}

func ValidateGainPPM(field string, gain int64) error {
	if gain < MinLegalGainPPM || gain > MaxLegalGainPPM {
		return e.Param(field, "500000..2000000 ppm (0.5x..2.0x)", "gain %d outside legal range", gain)
	}
	return nil
}

// ApplyGain applies a ppm fixed-point gain and records truncation if needed.
func ApplyGain(radiance, gainPPM int64, field string, rec *Recorder) (int64, error) {
	if err := ValidateGainPPM(field, gainPPM); err != nil {
		return 0, err
	}
	out, _, err := MulDivFloor(radiance, gainPPM, GainPPMUnity, field, rec, "apply_gain_ppm")
	return out, err
}
