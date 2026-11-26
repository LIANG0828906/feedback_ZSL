package kernel

import (
	"math"
	"math/bits"
)

// FixedPolicy declares the integer width and error bound used by all
// cross-unit conversions. The kernel never accumulates floating point error.
type FixedPolicy struct {
	BitWidth       int   `json:"bit_width"`
	FractionBits   uint8 `json:"fraction_bits"`
	ToleranceBytes int64 `json:"tolerance_bytes"`
	TolerancePPM   int64 `json:"tolerance_ppm"`
}

func DefaultFixedPolicy() FixedPolicy {
	return FixedPolicy{BitWidth: 64, FractionBits: 16, ToleranceBytes: 1, TolerancePPM: 10}
}

func (p FixedPolicy) Validate() error {
	if p.BitWidth != 32 && p.BitWidth != 64 {
		return NewBizError(CodeBudgetRange, "fixed point bit width must be 32 or 64, got %d", p.BitWidth)
	}
	if p.FractionBits > 30 {
		return NewBizError(CodeBudgetRange, "fraction bits must be <= 30, got %d", p.FractionBits)
	}
	if p.ToleranceBytes < 0 || p.TolerancePPM < 0 {
		return NewBizError(CodeTolerance, "tolerance must be non-negative")
	}
	return nil
}

// Fixed is a signed Qf FractionBits integer.
type Fixed struct {
	Raw          int64 `json:"raw"`
	FractionBits uint8 `json:"fraction_bits"`
}

func CheckedMul(a, b int64) (int64, bool) {
	if a == 0 || b == 0 {
		return 0, false
	}
	if a == math.MinInt64 || b == math.MinInt64 {
		return 0, true
	}
	if (a > 0 && b > 0 && a > math.MaxInt64/b) ||
		(a < 0 && b < 0 && a < math.MaxInt64/b) ||
		(a > 0 && b < 0 && b < math.MinInt64/a) ||
		(a < 0 && b > 0 && a < math.MinInt64/b) {
		return 0, true
	}
	return a * b, false
}

func SaturatingMul(a, b int64) (value int64, saturated bool) {
	v, overflow := CheckedMul(a, b)
	if !overflow {
		return v, false
	}
	if (a < 0) != (b < 0) {
		return math.MinInt64, true
	}
	return math.MaxInt64, true
}

// PermilleToPermyriad converts parts-per-thousand to parts-per-ten-thousand.
func PermilleToPermyriad(permille int64) (int64, error) {
	v, overflow := CheckedMul(permille, 10)
	if overflow {
		return 0, NewBizError(CodeOverflow, "permille %d cannot be widened to permyriad", permille)
	}
	return v, nil
}

func PermyriadToPermilleRounded(permyriad int64) (int64, int64, error) {
	if permyriad == math.MinInt64 {
		return 0, 0, NewBizError(CodeOverflow, "permyriad value is not representable")
	}
	negative := permyriad < 0
	v := permyriad
	if negative {
		v = -v
	}
	q, rem := v/10, v%10
	if rem >= 5 {
		q++
	}
	if negative {
		q = -q
		rem = -rem
	}
	return q, rem, nil
}

// KiBIntegerToBytes converts an integral KiB declaration to bytes.
func KiBIntegerToBytes(kib int64) (int64, error) {
	v, overflow := CheckedMul(kib, 1024)
	if overflow {
		return 0, NewBizError(CodeOverflow, "%d KiB overflows int64 bytes", kib)
	}
	return v, nil
}

// BytesToKiBFixed returns bytes / 1024 as a Qf fixed point value.
func BytesToKiBFixed(bytes int64, fractionBits uint8) (Fixed, error) {
	if bytes < 0 {
		return Fixed{}, NewBizError(CodeOverflow, "negative byte count cannot be converted to KiB fixed value")
	}
	if fractionBits > 30 {
		return Fixed{}, NewBizError(CodeOverflow, "fraction width %d is unsupported", fractionBits)
	}
	shift := uint(fractionBits)
	if bytes > math.MaxInt64>>shift {
		return Fixed{}, NewBizError(CodeOverflow, "%d bytes << %d overflows fixed raw", bytes, fractionBits)
	}
	raw := (bytes << shift) / 1024
	return Fixed{Raw: raw, FractionBits: fractionBits}, nil
}

// KiBFixedToBytesRounded converts a Qf KiB value back to whole bytes.
func KiBFixedToBytesRounded(value Fixed) (int64, error) {
	if value.FractionBits > 30 {
		return 0, NewBizError(CodeOverflow, "fraction width %d is unsupported", value.FractionBits)
	}
	scale := int64(1) << value.FractionBits
	quotient := value.Raw / scale
	rem := value.Raw % scale
	if rem < 0 {
		rem = -rem
	}
	if rem*2 >= scale {
		if value.Raw >= 0 {
			quotient++
		} else {
			quotient--
		}
	}
	return KiBIntegerToBytes(quotient)
}

func TicksToMilliseconds(ticks, msPerTick int64) (int64, error) {
	if msPerTick <= 0 {
		return 0, NewBizError(CodeTimeBasis, "milliseconds per tick must be positive")
	}
	v, overflow := CheckedMul(ticks, msPerTick)
	if overflow {
		return 0, NewBizError(CodeOverflow, "tick %d * %dms overflows", ticks, msPerTick)
	}
	return v, nil
}

func MillisecondsToTick(ms, msPerTick int64) (int64, bool, error) {
	if msPerTick <= 0 {
		return 0, false, NewBizError(CodeTimeBasis, "milliseconds per tick must be positive")
	}
	if ms < 0 {
		return 0, false, NewBizError(CodeTimeBasis, "timestamp %dms is negative", ms)
	}
	return ms / msPerTick, ms%msPerTick == 0, nil
}

// PermilleOf returns total * permille / 1000 using checked integer math.
func PermilleOf(total, permille int64) (int64, error) {
	return scaledFraction(total, permille, 1000)
}

// PermyriadOf returns total * permyriad / 10000 using checked integer math.
func PermyriadOf(total, permyriad int64) (int64, error) {
	return scaledFraction(total, permyriad, 10000)
}

func scaledFraction(total, numerator, denominator int64) (int64, error) {
	if denominator <= 0 {
		return 0, NewBizError(CodeOverflow, "invalid fraction denominator %d", denominator)
	}
	if total < 0 {
		return 0, NewBizError(CodeNegativeCapacity, "total capacity must be non-negative")
	}
	if numerator < 0 {
		return 0, NewBizError(CodeNegativeQuota, "fraction numerator must be non-negative")
	}
	if numerator != 0 && total != 0 && numerator > math.MaxInt64/total {
		return 0, NewBizError(CodeOverflow, "%d * %d overflows int64", total, numerator)
	}
	product := total * numerator
	return product / denominator, nil
}

// MulDiv128 is retained for critical Q64 cases where a 128-bit product is
// available. It explicitly reports saturation rather than wrapping.
func MulDiv128(a, b, divisor uint64) (uint64, bool) {
	hi, lo := bits.Mul64(a, b)
	if divisor == 0 || hi >= divisor {
		return math.MaxUint64, true
	}
	q, _ := bits.Div64(hi, lo, divisor)
	return q, false
}

func AbsDiff64(a, b int64) int64 {
	if a >= b {
		return a - b
	}
	return b - a
}

// WithinTolerance applies both an absolute byte bound and a parts-per-million
// relative bound. The absolute floor keeps zero-valued critical cases stable.
func WithinTolerance(got, want int64, policy FixedPolicy) bool {
	diff := AbsDiff64(got, want)
	limit := policy.ToleranceBytes
	if want > 0 && policy.TolerancePPM > 0 {
		relative := want / 1_000_000 * policy.TolerancePPM
		if want%1_000_000 != 0 {
			relative += (want % 1_000_000) * policy.TolerancePPM / 1_000_000
		}
		if relative > limit {
			limit = relative
		}
	}
	return diff <= limit
}
