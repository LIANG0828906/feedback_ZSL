// Package safemath provides the declared 64-bit integer arithmetic used by
// geometry and budget propagation.
package safemath

import (
	"math"
	"math/bits"

	"mirrorcast/internal/bizerror"
)

const DeclaredBits = 64

func Mul(a, b int64) (int64, error) {
	if a == 0 || b == 0 {
		return 0, nil
	}
	if a == math.MinInt64 || b == math.MinInt64 {
		return 0, bizerror.New(bizerror.EOverflow, "signed multiplication overflow")
	}
	if (a > 0 && b > 0 && a > math.MaxInt64/b) ||
		(a < 0 && b < 0 && a < math.MaxInt64/b) ||
		(a > 0 && b < 0 && b < math.MinInt64/a) ||
		(a < 0 && b > 0 && a < math.MinInt64/b) {
		return 0, bizerror.New(bizerror.EOverflow, "signed multiplication overflow")
	}
	return a * b, nil
}

func Add(a, b int64) (int64, error) {
	if b > 0 && a > math.MaxInt64-b {
		return 0, bizerror.New(bizerror.EOverflow, "signed addition overflow")
	}
	if b < 0 && a < math.MinInt64-b {
		return 0, bizerror.New(bizerror.EOverflow, "signed addition overflow")
	}
	return a + b, nil
}

func Sub(a, b int64) (int64, error) {
	if b > 0 && a < math.MinInt64+b {
		return 0, bizerror.New(bizerror.EOverflow, "signed subtraction overflow")
	}
	if b < 0 && a > math.MaxInt64+b {
		return 0, bizerror.New(bizerror.EOverflow, "signed subtraction overflow")
	}
	return a - b, nil
}

// MulDivRound computes round_half_up(a*b/den) without wrapping the product.
// All inputs must be non-negative and den must be positive.
func MulDivRound(a, b, den int64) (int64, error) {
	if a < 0 || b < 0 || den <= 0 {
		return 0, bizerror.New(bizerror.EInternal, "MulDivRound requires non-negative numerators and positive denominator")
	}
	hi, lo := bits.Mul64(uint64(a), uint64(b))
	if hi >= uint64(den) {
		return 0, bizerror.New(bizerror.EOverflow, "rounded product quotient exceeds int64")
	}
	prod128Hi, prod128Lo := hi, lo
	half := uint64(den / 2)
	lo, carry := bits.Add64(prod128Lo, half, 0)
	hi = prod128Hi + carry
	if hi >= uint64(den) {
		return 0, bizerror.New(bizerror.EOverflow, "rounded product quotient exceeds int64")
	}
	q, _ := bits.Div64(hi, lo, uint64(den))
	if q > math.MaxInt64 {
		return 0, bizerror.New(bizerror.EOverflow, "rounded result exceeds int64")
	}
	return int64(q), nil
}

// CeilDiv returns ceil(a/b) for non-negative a and positive b.
func CeilDiv(a, b int64) (int64, error) {
	if a < 0 || b <= 0 {
		return 0, bizerror.New(bizerror.EInternal, "CeilDiv requires a >= 0 and b > 0")
	}
	q := a / b
	if a%b != 0 {
		q2, err := Add(q, 1)
		if err != nil {
			return 0, err
		}
		q = q2
	}
	return q, nil
}

// SaturateInt32 narrows v using saturation. The boolean reports saturation.
func SaturateInt32(v int64) (int32, bool) {
	if v > math.MaxInt32 {
		return math.MaxInt32, true
	}
	if v < math.MinInt32 {
		return math.MinInt32, true
	}
	return int32(v), false
}

func AbsDiff(a, b int64) int64 {
	if a >= b {
		return a - b
	}
	return b - a
}

// SqrtExact returns sqrt(q) only when q is a non-negative perfect square.
func SqrtExact(q int64) (int64, bool) {
	if q < 0 {
		return 0, false
	}
	lo, hi := int64(0), int64(3037000499) // floor(sqrt(MaxInt64))
	for lo <= hi {
		mid := lo + (hi-lo)/2
		square := mid * mid
		if square == q {
			return mid, true
		}
		if square < q {
			lo = mid + 1
		} else {
			hi = mid - 1
		}
	}
	return 0, false
}
