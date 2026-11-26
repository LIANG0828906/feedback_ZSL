package cachebench

import (
	"fmt"
	"math"
	"math/big"
)

// FixedResult is a width-bounded integer conversion result. When saturation is
// true Value is the largest/smallest representable number for Bits.
type FixedResult struct {
	Value     int64
	Bits      int
	Saturated bool
	Overflow  bool
	Err       *DomainError
}

func widthBounds(bits int) (int64, int64, error) {
	if bits != 8 && bits != 16 && bits != 32 && bits != 64 {
		return 0, 0, NewError(CodeInvalidArgument, "unsupported fixed point width %d", bits).WithField("bits")
	}
	if bits == 64 {
		return math.MinInt64, math.MaxInt64, nil
	}
	max := int64(1)<<(bits-1) - 1
	min := -max - 1
	return min, max, nil
}

// ClampToWidth detects signed width overflow and saturates instead of wrapping.
func ClampToWidth(v int64, bits int) FixedResult {
	min, max, err := widthBounds(bits)
	if err != nil {
		de := err.(*DomainError)
		return FixedResult{Value: 0, Bits: bits, Overflow: true, Err: de}
	}
	if v > max {
		return FixedResult{Value: max, Bits: bits, Saturated: true, Overflow: true}
	}
	if v < min {
		return FixedResult{Value: min, Bits: bits, Saturated: true, Overflow: true}
	}
	return FixedResult{Value: v, Bits: bits}
}

func safeMul(a, b int64) (int64, bool) {
	if a == 0 || b == 0 {
		return 0, true
	}
	if a == math.MinInt64 || b == math.MinInt64 {
		if (a == math.MinInt64 && (b != 1)) || (b == math.MinInt64 && (a != 1)) {
			return 0, false
		}
	}
	res := a * b
	if res/a != b {
		return 0, false
	}
	return res, true
}

// ConvertScale converts an integer expressed on FromScale to ToScale. Exact
// enlargements multiply; reductions use nearest rounding. All arithmetic is
// checked before applying the declared output width.
func ConvertScale(value, fromScale, toScale int64, bits int) FixedResult {
	if fromScale <= 0 || toScale <= 0 {
		err := NewError(CodeInvalidArgument, "fixed point scale must be positive").
			WithField("scale").
			WithContext("from", fmt.Sprint(fromScale)).
			WithContext("to", fmt.Sprint(toScale))
		return FixedResult{Err: err}
	}
	var out int64
	if toScale >= fromScale {
		factor := toScale / fromScale
		if toScale%fromScale != 0 {
			err := NewError(CodeInvalidArgument, "scale conversion is not integral").
				WithField("toScale").
				WithContext("from", fmt.Sprint(fromScale)).
				WithContext("to", fmt.Sprint(toScale))
			return FixedResult{Err: err}
		}
		v, ok := safeMul(value, factor)
		if !ok {
			r := ClampToWidth(math.MaxInt64, bits)
			r.Overflow = true
			r.Err = NewError(CodeFixedOverflow, "multiply overflow")
			return r
		}
		out = v
	} else {
		div := fromScale / toScale
		if fromScale%toScale != 0 {
			err := NewError(CodeInvalidArgument, "scale conversion is not integral").
				WithField("fromScale").
				WithContext("from", fmt.Sprint(fromScale)).
				WithContext("to", fmt.Sprint(toScale))
			return FixedResult{Err: err}
		}
		// Nearest rounding, symmetric for negative values.
		if value >= 0 {
			out = (value + div/2) / div
		} else {
			out = -((-value + div/2) / div)
		}
	}
	r := ClampToWidth(out, bits)
	if r.Overflow && r.Err == nil {
		r.Err = NewError(CodeFixedOverflow, "value exceeds declared %d-bit width", bits).
			WithContext("value", fmt.Sprint(value))
	}
	return r
}

func PermilleToPermyriad(v int64, bits int) FixedResult {
	return ConvertScale(v, ScalePermille, ScalePermyriad, bits)
}

func PermyriadToPermille(v int64, bits int) FixedResult {
	return ConvertScale(v, ScalePermyriad, ScalePermille, bits)
}

// TicksToMS converts logical ticks using the normalized tick duration.
func TicksToMS(ticks, tickMS int64, bits int) FixedResult {
	if tickMS <= 0 {
		return FixedResult{Err: NewError(CodeInvalidArgument, "tickMS must be positive").WithField("tickMS")}
	}
	v, ok := safeMul(ticks, tickMS)
	if !ok {
		r := ClampToWidth(math.MaxInt64, bits)
		r.Err = NewError(CodeFixedOverflow, "tick to ms overflow")
		return r
	}
	r := ClampToWidth(v, bits)
	if r.Overflow {
		r.Err = NewError(CodeFixedOverflow, "tick result exceeds width")
	}
	return r
}

// MSToTicks floors to whole ticks and reports the lost milliseconds.
func MSToTicks(ms, tickMS int64, bits int) (FixedResult, int64) {
	if tickMS <= 0 {
		return FixedResult{Err: NewError(CodeInvalidArgument, "tickMS must be positive").WithField("tickMS")}, 0
	}
	rem := ms % tickMS
	if rem < 0 {
		rem += tickMS
	}
	r := ClampToWidth(ms/tickMS, bits)
	if r.Overflow {
		r.Err = NewError(CodeFixedOverflow, "tick result exceeds width")
	}
	return r, rem
}

// FixedSecondsToMS converts a fixed-point seconds value whose unit fraction is
// fracScale. Example: value=3, fracScale=1000 means 0.003s = 3ms.
func FixedSecondsToMS(value, fracScale int64, bits int) FixedResult {
	return ConvertScale(value*1000, fracScale, 1, bits)
}

// RatePerTickToPerSecond converts requests per tick to requests per second.
// The result remains fixed point with rateScale decimal resolution.
func RatePerTickToPerSecond(rateFixed, rateScale, tickMS int64, bits int) FixedResult {
	if tickMS <= 0 || rateScale <= 0 {
		return FixedResult{Err: NewError(CodeInvalidArgument, "invalid rate conversion argument")}
	}
	numerator, ok := safeMul(rateFixed, 1000)
	if !ok {
		r := ClampToWidth(math.MaxInt64, bits)
		r.Err = NewError(CodeFixedOverflow, "rate numerator overflow")
		return r
	}
	out := numerator / tickMS
	r := ClampToWidth(out, bits)
	if r.Overflow {
		r.Err = NewError(CodeFixedOverflow, "rate exceeds width")
	}
	return r
}

// WithinTolerancePPM compares got and expected using a relative PPM tolerance.
// A zero expected value requires exact zero unless got is also zero.
func WithinTolerancePPM(got, expected, tolerancePPM int64) bool {
	if got == expected {
		return true
	}
	if expected == 0 {
		return false
	}
	diff := got - expected
	if diff < 0 {
		diff = -diff
	}
	lhs := new(big.Int).Mul(big.NewInt(diff), big.NewInt(PPMScale))
	rhs := new(big.Int).Mul(big.NewInt(abs64(expected)), big.NewInt(tolerancePPM))
	return lhs.Cmp(rhs) <= 0
}

func abs64(v int64) int64 {
	if v < 0 {
		return -v
	}
	return v
}

// ErrorRatePPM computes errors/requests * 1e6 without floating point.
func ErrorRatePPM(errors, requests int64) (int64, bool) {
	if requests <= 0 {
		return 0, true
	}
	v, ok := safeMul(errors, PPMScale)
	if !ok {
		return math.MaxInt64, false
	}
	return v / requests, true
}

// AverageFixed computes total/count * fractionScale with rounding.
func AverageFixed(total, count, fractionScale int64, bits int) FixedResult {
	if count <= 0 {
		return FixedResult{Value: 0, Bits: bits}
	}
	v, ok := safeMul(total, fractionScale)
	if !ok {
		r := ClampToWidth(math.MaxInt64, bits)
		r.Err = NewError(CodeFixedOverflow, "average numerator overflow")
		return r
	}
	out := (v + count/2) / count
	r := ClampToWidth(out, bits)
	if r.Overflow {
		r.Err = NewError(CodeFixedOverflow, "average exceeds width")
	}
	return r
}
