// Package fixedpoint contains the declared-width integer arithmetic used by
// weight conversion and tolerance checks. It never uses floating point in
// scheduling or window decisions.
package fixedpoint

import (
	"math"

	"multiplexgw/internal/bizerr"
)

const (
	// FractionBits is the Q16 fractional width used in reports.
	FractionBits = 16
	Scale        = int64(1) << FractionBits
	// WindowBits mirrors HTTP/2's 31-bit flow-control window.
	WindowBits = 31
	MaxWindow  = int64(math.MaxInt32)
	// WeightBits declares the accepted unsigned weight range width.
	WeightBits       = 16
	MaxPermille      = int64(1000)
	MaxPermyriad     = int64(10000)
	DefaultTolerance = Q(2) // two Q16 ULP, including nearest rounding
)

// Q is a signed Q16.16 fixed-point value stored in 64 bits.
type Q int64

func FromInt(v int64) Q { return Q(v * Scale) }

func (q Q) Float() float64 { return float64(q) / float64(Scale) }

// CheckedAdd performs int64 addition and reports overflow without saturation.
func CheckedAdd(a, b int64) (int64, bool) {
	if b > 0 && a > math.MaxInt64-b {
		return 0, false
	}
	if b < 0 && a < math.MinInt64-b {
		return 0, false
	}
	return a + b, true
}

// CheckedMul performs int64 multiplication and reports overflow.
func CheckedMul(a, b int64) (int64, bool) {
	if a == 0 || b == 0 {
		return 0, true
	}
	r := a * b
	if r/b != a {
		return 0, false
	}
	return r, true
}

// SaturatingAddWindow adds delta to a 31-bit window and saturates at cap.
// The boolean is false when the requested result exceeded cap or overflowed.
func SaturatingAddWindow(current, delta, cap int64) (int64, bool) {
	if current < 0 || delta < 0 || cap < 0 || cap > MaxWindow {
		return 0, false
	}
	next, ok := CheckedAdd(current, delta)
	if !ok || next > cap {
		return cap, true
	}
	return next, false
}

// PermilleToPermyriad exactly maps the two declared weight scales.
func PermilleToPermyriad(v int64) (int64, error) {
	if v < 0 || v > MaxPermille {
		return 0, bizerr.New(bizerr.WeightRange, "weight", "permille weight %d outside [0,%d]", v, MaxPermille)
	}
	r, ok := CheckedMul(v, 10)
	if !ok {
		return 0, bizerr.New(bizerr.FixedOverflow, "weight", "permille conversion overflow").WithContext("value", v)
	}
	return r, nil
}

// Share calculates weight/total in Q16. Rounding is nearest-even-free integer
// half-up, deterministic on every platform.
func Share(weight, total int64) (Q, error) {
	if weight < 0 || total <= 0 || weight > total {
		return 0, bizerr.New(bizerr.WeightRange, "fixedpoint.share", "invalid weight share %d/%d", weight, total)
	}
	scaled, ok := CheckedMul(weight, Scale)
	if !ok {
		return 0, bizerr.New(bizerr.FixedOverflow, "fixedpoint.share", "weight share overflow").
			WithContext("weight", weight).WithContext("scale", Scale)
	}
	// Half-up rounding: floor((n*2 + den) / (2*den)).
	n2, ok := CheckedMul(scaled, 2)
	if !ok {
		return 0, bizerr.New(bizerr.FixedOverflow, "fixedpoint.share", "rounding overflow")
	}
	return Q((n2 + total) / (2 * total)), nil
}

// AllocatedShare reports allocated bytes converted to a Q16 fraction of total.
func AllocatedShare(allocated, totalBytes int64) (Q, error) {
	if totalBytes <= 0 {
		return 0, nil
	}
	return Share(allocated, totalBytes)
}

// WithinTolerance compares a computed and truth Q value under tol.
func WithinTolerance(computed, truth, tol Q) bool {
	d := computed - truth
	if d < 0 {
		d = -d
	}
	return d <= tol
}
