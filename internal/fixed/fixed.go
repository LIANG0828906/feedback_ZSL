// Package fixed implements declared-width deterministic fixed-point conversion.
//
// All project arithmetic is ultimately stored in integer bytes/ticks. The
// helpers here make cross-schema scaling explicit and report overflow instead
// of silently wrapping machine integers.
package fixed

import (
	"math"
	"math/bits"

	"quota-cluster/internal/bizerror"
)

const (
	// WidthBits is the declared signed machine width used by project values.
	WidthBits = 63
	// FractionScale is 10^9 and keeps KiB/tick fractional diagnostics below
	// one unit after rounding for the generated critical cases.
	FractionScale int64 = 1_000_000_000
)

// Result is a saturation-aware conversion outcome.
type Result struct {
	Value       int64  `json:"value"`
	Exact       bool   `json:"exact"`
	Overflow    bool   `json:"overflow"`
	Saturated   bool   `json:"saturated"`
	Code        string `json:"code,omitempty"`
	Remainder   int64  `json:"remainder,omitempty"`
	Denominator int64  `json:"denominator,omitempty"`
}

func overflowResult(path string) Result {
	return Result{
		Value:     math.MaxInt64,
		Overflow:  true,
		Saturated: true,
		Code:      bizerror.CodeScaleOverflow,
	}
}

// MulDivRound computes round(x * numerator / denominator) without an
// intermediate 64-bit wrap. Inputs must be non-negative.
func MulDivRound(x, numerator, denominator int64) (int64, bool) {
	v, _, _, overflow := mulDivRoundRem(x, numerator, denominator)
	return v, overflow
}

func mulDivRoundRem(x, numerator, denominator int64) (value int64, rounded bool, remainder uint64, overflow bool) {
	if denominator <= 0 {
		return 0, false, 0, false
	}
	if x < 0 || numerator < 0 {
		return 0, false, 0, false
	}
	hi, lo := bits.Mul64(uint64(x), uint64(numerator))
	if hi >= uint64(denominator) {
		return math.MaxInt64, false, 0, true
	}
	q, rem64 := bits.Div64(hi, lo, uint64(denominator))
	rounded = rem64*2 >= uint64(denominator)
	if rounded {
		if q == math.MaxInt64 {
			return math.MaxInt64, true, rem64, true
		}
		q++
	}
	if q > math.MaxInt64 {
		return math.MaxInt64, rounded, rem64, true
	}
	return int64(q), rounded, rem64, false
}

// CheckedMul returns a*b and false when the signed result is representable.
func CheckedMul(a, b int64) (int64, bool) {
	if a == 0 || b == 0 {
		return 0, true
	}
	if a > math.MaxInt64/b || a < math.MinInt64/b {
		return 0, false
	}
	v := a * b
	if (a == math.MinInt64 && b == -1) || v/b != a {
		return 0, false
	}
	return v, true
}

// ScalePermilleToBasisPoints maps 1/1000 units to 1/10000 units.
func ScalePermilleToBasisPoints(permille int64) Result {
	v, ok := CheckedMul(permille, 10)
	if !ok || permille < 0 {
		return overflowResult("/ratio/permille")
	}
	return Result{Value: v, Exact: true}
}

// ScaleBasisPointsToPermille maps 1/10000 units to 1/1000 units. Remainder is
// retained so callers can verify rounding error bounds.
func ScaleBasisPointsToPermille(bp int64) Result {
	if bp < 0 {
		return overflowResult("/ratio/basis_points")
	}
	q := bp / 10
	rem := bp % 10
	if rem >= 5 {
		if q == math.MaxInt64 {
			return overflowResult("/ratio/basis_points")
		}
		q++
	}
	return Result{Value: q, Exact: rem == 0, Remainder: rem, Denominator: 10}
}

// BytesToKiB maps bytes to a fixed-point KiB value at FractionScale.
func BytesToKiB(bytes int64) Result {
	if bytes < 0 {
		return overflowResult("/capacity/bytes")
	}
	v, overflow := MulDivRound(bytes, FractionScale, 1024)
	if overflow {
		return overflowResult("/capacity/bytes")
	}
	exact := bytes%1024 == 0
	return Result{Value: v, Exact: exact, Remainder: bytes % 1024, Denominator: 1024}
}

// KiBToBytes maps an integral KiB declaration to exact bytes.
func KiBToBytes(kib int64) Result {
	v, ok := CheckedMul(kib, 1024)
	if !ok || kib < 0 {
		return overflowResult("/capacity/kib")
	}
	return Result{Value: v, Exact: true}
}

// TicksToMillis maps logical ticks to the schema's millisecond basis.
func TicksToMillis(tick, tickMillis int64) Result {
	v, ok := CheckedMul(tick, tickMillis)
	if !ok || tick < 0 || tickMillis <= 0 {
		return overflowResult("/time/tick")
	}
	return Result{Value: v, Exact: true}
}

// MillisToTicksFixed maps milliseconds to fixed-point ticks at FractionScale.
func MillisToTicksFixed(millis, tickMillis int64) Result {
	if millis < 0 || tickMillis <= 0 {
		return overflowResult("/time/millis")
	}
	v, overflow := MulDivRound(millis, FractionScale, tickMillis)
	if overflow {
		return overflowResult("/time/millis")
	}
	return Result{Value: v, Exact: millis%tickMillis == 0, Remainder: millis % tickMillis, Denominator: tickMillis}
}

// RatioToBytes converts numerator/scale of a declared byte basis to bytes.
func RatioToBytes(numerator, scale, basisBytes int64) Result {
	if numerator < 0 || scale <= 0 || basisBytes < 0 {
		return overflowResult("/ratio")
	}
	v, _, rem, overflow := mulDivRoundRem(numerator, basisBytes, scale)
	if overflow {
		return overflowResult("/ratio")
	}
	return Result{
		Value:       v,
		Exact:       rem == 0,
		Remainder:   int64(rem),
		Denominator: scale,
	}
}

// WithinByteTolerance reports whether |actual-expected| <= tolerance.
func WithinByteTolerance(actual, expected, tolerance int64) bool {
	d := actual - expected
	if d < 0 {
		d = -d
	}
	return d <= tolerance
}
