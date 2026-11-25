// Package fixedpoint implements every cross-schema numeric conversion with
// declared bit-width integer arithmetic. No floating point is used anywhere:
// tick/ms/s durations and per-mille/per-ten-thousand budget ratios are all
// integers, and every operation explicitly reports overflow and saturation.
package fixedpoint

import (
	"math/bits"

	"coldkernel/internal/errcode"
)

// Canonical scales and time factors used across the whole kernel.
const (
	ScalePermille      int64 = 1000  // ‰
	ScalePerTenThou    int64 = 10000 // ‱
	BudgetCanonScale   int64 = ScalePerTenThou
	MsPerSecond        int64 = 1000
	DefaultTickMs      int64 = 100 // 1 logical tick == 100 ms in the samples
)

// Bounds for a declared signed integer bit width.
func Bounds(width int) (min, max int64) {
	if width >= 64 {
		return minInt64, maxInt64
	}
	max = int64(1)<<(width-1) - 1
	min = -int64(1) << (width - 1)
	return min, max
}

const (
	maxInt64 = int64(^uint64(0) >> 1)
	minInt64 = -maxInt64 - 1
)

// Result is a checked arithmetic outcome.
type Result struct {
	Value      int64
	Overflowed bool
	Saturated  bool
	// Remainder is the discarded fractional part of a division conversion
	// (always non-negative). It feeds the accumulated conversion error.
	Remainder int64
}

func clamp(width int, v int64) (int64, bool) {
	min, max := Bounds(width)
	if v < min {
		return min, true
	}
	if v > max {
		return max, true
	}
	return v, false
}

// Add returns a+b within the declared width, flagging overflow/saturation.
func Add(width int, a, b int64) Result {
	s, carry := bits.Add64(uint64(a), uint64(b), 0)
	v := int64(s)
	// Signed overflow: positive+positive yielding negative, or negative+
	// negative yielding positive. The carry bit alone is not sufficient.
	overflow := (a >= 0 && b >= 0 && v < 0) || (a < 0 && b < 0 && v >= 0)
	_ = carry
	if overflow {
		min, max := Bounds(width)
		if a < 0 && b < 0 {
			return Result{Value: min, Overflowed: true, Saturated: true}
		}
		return Result{Value: max, Overflowed: true, Saturated: true}
	}
	if c, sat := clamp(width, v); sat {
		return Result{Value: c, Saturated: true}
	}
	return Result{Value: v}
}

// Sub returns a-b within the declared width.
func Sub(width int, a, b int64) Result {
	d, borrow := bits.Sub64(uint64(a), uint64(b), 0)
	v := int64(d)
	// Signed overflow: positive minus negative yielding negative, or
	// negative minus positive yielding positive/non-negative.
	overflow := (a >= 0 && b < 0 && v < 0) || (a < 0 && b >= 0 && v >= 0)
	_ = borrow
	if overflow {
		min, max := Bounds(width)
		if a < 0 && b >= 0 {
			return Result{Value: min, Overflowed: true, Saturated: true}
		}
		return Result{Value: max, Overflowed: true, Saturated: true}
	}
	if c, sat := clamp(width, v); sat {
		return Result{Value: c, Saturated: true}
	}
	return Result{Value: v}
}

// Mul returns a*b within the declared width.
func Mul(width int, a, b int64) Result {
	hi, lo := bits.Mul64(uint64(a), uint64(b))
	v := int64(lo)
	// Signed overflow: the high 64 bits must equal the sign extension.
	overflow := hi != uint64(v>>63)
	if overflow {
		// Saturate at the declared width bound. The sign of the true result
		// follows the xor of the operand signs.
		min, max := Bounds(width)
		if (a < 0) != (b < 0) {
			return Result{Value: min, Overflowed: true, Saturated: true}
		}
		return Result{Value: max, Overflowed: true, Saturated: true}
	}
	if c, sat := clamp(width, v); sat {
		return Result{Value: c, Saturated: true}
	}
	return Result{Value: v}
}

// Div returns a/b with explicit remainder (truncated toward zero). Division
// by zero is reported as an overflow instead of panicking.
func Div(width int, a, b int64) Result {
	if b == 0 {
		min, _ := Bounds(width)
		return Result{Value: min, Overflowed: true, Saturated: true}
	}
	if a == minInt64 && b == -1 {
		_, max := Bounds(width)
		return Result{Value: max, Overflowed: true, Saturated: true}
	}
	q := a / b
	r := a % b
	if r < 0 {
		r = -r
	}
	c, sat := clamp(width, q)
	return Result{Value: c, Saturated: sat, Remainder: r}
}

// MustValue returns the value or an errcode error when overflow/saturation
// occurred outside an explicitly tolerated conversion.
func MustValue(r Result, field string, attrs ...any) (int64, error) {
	if r.Overflowed {
		return r.Value, errcode.New(errcode.EOverflow, "integer overflow in "+field, attrs...)
	}
	return r.Value, nil
}

// MsToTicks converts a millisecond duration to whole logical ticks. The
// discarded sub-tick remainder is returned so callers can accumulate the
// conversion error and check it against the declared tolerance.
func MsToTicks(width int, ms, tickMs int64) Result {
	return Div(width, ms, tickMs)
}

// TicksToMs converts ticks back to milliseconds (exact when tickMs divides).
func TicksToMs(width int, ticks, tickMs int64) Result {
	return Mul(width, ticks, tickMs)
}

// SecondsToMs converts a second duration to milliseconds.
func SecondsToMs(width int, seconds int64) Result {
	return Mul(width, seconds, MsPerSecond)
}

// Accumulator tracks the accumulated absolute conversion error against a
// declared fixed-point tolerance.
type Accumulator struct {
	Width     int
	Tolerance int64
	sum       int64
	bad       bool
}

// AddRemainder folds a conversion remainder into the running error.
func (a *Accumulator) AddRemainder(r int64) error {
	res := Add(a.Width, a.sum, r)
	a.sum = res.Value
	if res.Overflowed {
		a.bad = true
		return errcode.New(errcode.EOverflow, "error accumulator overflow")
	}
	if a.sum > a.Tolerance {
		a.bad = true
	}
	return nil
}

// WithinTolerance reports whether the accumulated error stays in bounds.
func (a *Accumulator) WithinTolerance() bool { return !a.bad && a.sum <= a.Tolerance }

// Sum returns the accumulated absolute conversion error.
func (a *Accumulator) Sum() int64 { return a.sum }

// WithinTolerance checks a single observed-vs-expected pair directly.
func WithinTolerance(got, want, tolerance int64) bool {
	d := got - want
	if d < 0 {
		d = -d
	}
	return d <= tolerance
}

// BudgetRatio converts a cold-start budget ratio into the canonical
// per-ten-thousand scale. Input scale must be 1000 (‰) or 10000 (‱).
func BudgetRatio(width int, value, inputScale int64) (Result, error) {
	switch inputScale {
	case ScalePerTenThou:
		if value < 0 || value > ScalePerTenThou {
			return Result{}, errcode.New(errcode.EBadBudget, "budget outside ‱ range",
				"value", value, "scale", inputScale)
		}
		c, sat := clamp(width, value)
		return Result{Value: c, Saturated: sat}, nil
	case ScalePermille:
		if value < 0 || value > ScalePermille {
			return Result{}, errcode.New(errcode.EBadBudget, "budget outside ‰ range",
				"value", value, "scale", inputScale)
		}
		// ‱ = ‰ * 10, an exact scale change with no rounding error.
		r := Mul(width, value, 10)
		if r.Overflowed {
			return r, errcode.New(errcode.EOverflow, "budget scale conversion overflow")
		}
		return r, nil
	default:
		return Result{}, errcode.New(errcode.EBadBudget, "unknown budget scale",
			"scale", inputScale)
	}
}

// AbsoluteFromRemaining maps the v2 "remaining available concurrency"
// semantics back to an absolute limit: absolute = occupied + remaining.
func AbsoluteFromRemaining(width int, occupied, remaining int64) Result {
	return Add(width, occupied, remaining)
}
