package queryopt

import (
	"math"
	"math/bits"
)

// Fixed is a signed Q32.32 integer. Selectivity uses the [0, 1] interval,
// but saturation helpers preserve explicit overflow information.
type Fixed int64

const (
	FixedZero Fixed = 0
	FixedOne  Fixed = Fixed(FixedScale)
	FixedMax  Fixed = Fixed(math.MaxInt64)
)

func FixedFromInt64(v int64) (Fixed, bool) {
	if v > math.MaxInt32 || v < math.MinInt32 {
		return FixedMax, true
	}
	return Fixed(v * FixedScale), false
}

// FixedFromRatio returns num/den in Q32.32. A non-positive denominator is an
// input fault. Overflow saturates to FixedMax and reports it.
func FixedFromRatio(num, den int64) (Fixed, bool) {
	if den <= 0 {
		return 0, true
	}
	if num < 0 {
		v, overflow := FixedFromRatio(-num, den)
		return -v, overflow
	}
	// num is normally small enough; split whole and remainder before scaling.
	whole := num / den
	rem := num % den
	if whole > math.MaxInt32 {
		return FixedMax, true
	}
	scaledRem := rem * FixedScale
	frac := scaledRem / den
	if (scaledRem%den)*2 >= den {
		frac++
	}
	raw := whole*FixedScale + frac
	if raw > math.MaxInt64 || raw < 0 {
		return FixedMax, true
	}
	return Fixed(raw), false
}

func (f Fixed) Permyriad() int64 {
	if f <= 0 {
		return 0
	}
	return (int64(f)*10_000 + FixedScale/2) / FixedScale
}

func FixedFromPermille(v int64) (Fixed, bool) {
	if v < 0 || v > 1000 {
		return FixedMax, true
	}
	return Fixed(v * FixedScale / 1000), false
}

func FixedFromPermyriad(v int64) (Fixed, bool) {
	if v < 0 || v > 10_000 {
		return FixedMax, true
	}
	return Fixed(v * FixedScale / 10_000), false
}

// Mul returns a*b in Q32.32 and an overflow/saturation flag.
func (f Fixed) Mul(g Fixed) (Fixed, bool) {
	if f == 0 || g == 0 {
		return 0, false
	}
	negative := (f < 0) != (g < 0)
	af := uint64(abs64(int64(f)))
	ag := uint64(abs64(int64(g)))
	hi, lo := bits.Mul64(af, ag)
	// The Q32 result is (hi:lo) >> 32. For bounded selectivity values hi is 0
	// or 1; the explicit hi threshold keeps the operation total.
	if hi > math.MaxInt32 {
		if negative {
			return -FixedMax, true
		}
		return FixedMax, true
	}
	raw := int64((hi << 32) | (lo >> 32))
	if raw < 0 {
		if negative {
			return -FixedMax, true
		}
		return FixedMax, true
	}
	if negative {
		return -Fixed(raw), false
	}
	return Fixed(raw), false
}

func (f Fixed) Add(g Fixed) (Fixed, bool) {
	s := int64(f) + int64(g)
	if (int64(f) > 0 && int64(g) > 0 && s < 0) || (int64(f) < 0 && int64(g) < 0 && s > 0) {
		if f < 0 {
			return -FixedMax, true
		}
		return FixedMax, true
	}
	return Fixed(s), false
}

func (f Fixed) Complement() Fixed {
	if f <= 0 {
		return FixedOne
	}
	if f >= FixedOne {
		return 0
	}
	return FixedOne - f
}

// RoundedRows maps a selectivity to an estimated row count. Multiplication is
// checked; saturation is reported rather than silently wrapping.
func RoundedRows(rows int64, sel Fixed) (int64, bool) {
	if rows <= 0 || sel <= 0 {
		return 0, false
	}
	if sel == FixedOne {
		return rows, false
	}
	hi, lo := bits.Mul64(uint64(rows), uint64(sel))
	// Round: add 2^31 to the 128-bit product before shifting by 32.
	lo2, carry := bits.Add64(lo, 1<<31, 0)
	hi2, _ := bits.Add64(hi, 0, carry)
	rounded, overflow := shiftRightQ32(hi2, lo2)
	if overflow || rounded > math.MaxInt64 {
		return math.MaxInt64, true
	}
	return rounded, false
}

func shiftRightQ32(hi, lo uint64) (int64, bool) {
	if hi > math.MaxInt32 {
		return math.MaxInt64, true
	}
	v := int64((hi << 32) | (lo >> 32))
	if v < 0 {
		return math.MaxInt64, true
	}
	return v, false
}

// CheckedMul scales a row/cost product and saturates at MaxInt64.
func CheckedMul(a, b int64) (int64, bool) {
	if a == 0 || b == 0 {
		return 0, false
	}
	if a < 0 || b < 0 {
		return 0, true
	}
	hi, lo := bits.Mul64(uint64(a), uint64(b))
	if hi != 0 || lo > math.MaxInt64 {
		return math.MaxInt64, true
	}
	return int64(lo), false
}

func CheckedAdd(a, b int64) (int64, bool) {
	if b > 0 && a > math.MaxInt64-b {
		return math.MaxInt64, true
	}
	if b < 0 && a < math.MinInt64-b {
		return 0, true
	}
	return a + b, false
}

func abs64(v int64) uint64 {
	if v >= 0 {
		return uint64(v)
	}
	if v == math.MinInt64 {
		return uint64(math.MaxInt64) + 1
	}
	return uint64(-v)
}
