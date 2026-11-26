// Package fixedint implements the declared-width (signed 64-bit) fixed-point
// arithmetic used for every resource and weight conversion. All operations
// explicitly detect overflow; callers can either receive the overflow signal
// or request saturation. No floating point math is used anywhere.
package fixedint

import (
	"math/bits"
)

// DeclaredWidth is the fixed-point bit width used throughout the component.
const DeclaredWidth = 64

// Scale constants.
const (
	MilliPerCore int64 = 1000
	Permille     int64 = 1000
	BPS          int64 = 10000
)

// Max and Min of the declared signed width.
const (
	Max = int64(^uint64(0) >> 1)
	Min = -Max - 1
)

// Add returns a+b and whether the operation overflowed int64.
func Add(a, b int64) (int64, bool) {
	if b > 0 && a > Max-b {
		return saturate(a, b), true
	}
	if b < 0 && a < Min-b {
		return saturate(a, b), true
	}
	return a + b, false
}

// Mul returns a*b and whether the operation overflowed int64.
func Mul(a, b int64) (int64, bool) {
	neg := (a < 0) != (b < 0)
	x, y := abs64(a), abs64(b)
	hi, lo := bits.Mul64(x, y)
	// In-range magnitude fits in 63 bits: the high word must be zero and the
	// low word must not set the sign bit (except the exact Min magnitude).
	overflow := hi != 0 || lo > uint64(Max)
	if overflow {
		if neg {
			return Min, true
		}
		return Max, true
	}
	result := int64(lo)
	if neg {
		result = -result
	}
	return result, false
}

func abs64(v int64) uint64 {
	if v >= 0 {
		return uint64(v)
	}
	return uint64(-v)
}

// Scale computes numerator*amount/denominator rounded half away from zero and
// reports overflow. denominator must be positive. The multiplication is done
// in 128 bits only to detect int64 overflow; the returned value is clamped to
// the declared width when overflow is signalled.
func Scale(amount, numerator, denominator int64) (int64, bool) {
	if denominator <= 0 {
		return 0, true
	}
	hi, lo := bits.Mul64(uint64(amount), uint64(numerator))
	positive := (amount >= 0) == (numerator >= 0)
	// Quotient of the 128-bit product by denominator, kept in int64 range.
	var q uint64
	var rem uint64
	if positive {
		q, rem = bits.Div64(hi, lo, uint64(denominator))
		if q > uint64(Max) {
			return Max, true
		}
		result := int64(q)
		// half away from zero: round up when rem*2 >= denominator.
		if rem != 0 && rem >= uint64(denominator)-rem {
			r, ov := Add(result, 1)
			if ov {
				return Max, true
			}
			result = r
		}
		return result, false
	}
	// Negative product: divide the absolute magnitude then reapply sign.
	absHi, absLo := negate128(hi, lo)
	q, rem = bits.Div64(absHi, absLo, uint64(denominator))
	if q > uint64(Max)+1 {
		return Min, true
	}
	magnitude := int64(q)
	if rem != 0 && rem >= uint64(denominator)-rem {
		magnitude++
	}
	limit := uint64(Max) + 1
	if uint64(magnitude) == limit {
		return Min, false // exactly Min, representable
	}
	return -magnitude, false
}

func negate128(hi, lo uint64) (uint64, uint64) {
	lo2 := ^lo + 1
	hi2 := ^hi
	if lo2 == 0 {
		hi2++
	}
	return hi2, lo2
}

func saturate(a, b int64) int64 {
	if (a >= 0) == (b >= 0) {
		if a >= 0 {
			return Max
		}
		return Min
	}
	return 0
}

// Saturate clamps v into the declared width (identity for in-range values).
func Saturate(v int64) int64 { return v }

// Rational is an exact signed decimal rational num/den where den is a power of
// ten. It is the parsed form of a decimal or percentage string.
type Rational struct {
	Num       int64
	Den       int64 // strictly positive power of ten
	IsPercent bool
}

// ParseRational parses an exact decimal or percentage string without floats.
// Accepted grammar: [+-]? digits ['.' digits] ['%']. No exponent, no units.
func ParseRational(s string) (Rational, error) {
	neg := false
	i := 0
	if len(s) > 0 && (s[0] == '+' || s[0] == '-') {
		neg = s[0] == '-'
		i = 1
	}
	pct := false
	if len(s) > 0 && s[len(s)-1] == '%' {
		pct = true
		s = s[:len(s)-1]
	}
	intPart := int64(0)
	frac := int64(0)
	den := int64(1)
	seenDigit, seenDot := false, false
	for ; i < len(s); i++ {
		c := s[i]
		switch {
		case c >= '0' && c <= '9':
			seenDigit = true
			if seenDot {
				v, ov1 := Mul(frac, 10)
				v2, ov2 := Add(v, int64(c-'0'))
				den *= 10
				if ov1 || ov2 || den < 0 {
					return Rational{}, errRange(s)
				}
				frac = v2
			} else {
				v, ov1 := Mul(intPart, 10)
				v2, ov2 := Add(v, int64(c-'0'))
				if ov1 || ov2 {
					return Rational{}, errRange(s)
				}
				intPart = v2
			}
		case c == '.' && !seenDot:
			seenDot = true
		default:
			return Rational{}, errSyntax(s)
		}
	}
	if !seenDigit {
		return Rational{}, errSyntax(s)
	}
	whole, ov := Mul(intPart, den)
	num, ov2 := Add(whole, frac)
	if ov || ov2 {
		return Rational{}, errRange(s)
	}
	if neg {
		num = -num
	}
	return Rational{Num: num, Den: den, IsPercent: pct}, nil
}

type parseError struct{ msg string }

func (e *parseError) Error() string { return e.msg }

func errSyntax(s string) error { return &parseError{"invalid fixed-point syntax: " + s} }
func errRange(s string) error  { return &parseError{"fixed-point value out of int64 range: " + s} }

// IsParseError reports whether err originates in the rational parser.
func IsParseError(err error) bool {
	_, ok := err.(*parseError)
	return ok
}
