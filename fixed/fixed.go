// Package fixed contains the declared-width integer arithmetic used for
// schema scale conversion and quota settlement.
package fixed

import (
	"errors"
	"math"
	"math/big"
)

const (
	// WidthBits is the declared integer width for persisted quota values.
	WidthBits = 64
	// Scale is a decimal fixed-point scale. Rates are stored in parts per
	// million (PPM), which exactly contains both per-mille and per-myriad.
	Scale int64 = 1_000_000
	// MaxRatePPM is 100%.
	MaxRatePPM int64 = Scale
	// KiB is the exact byte width of one kibibyte.
	KiB int64 = 1024
	// DefaultToleranceBytes is the declared settlement tolerance. Integer
	// floor conversion can introduce less than one byte of rounding error.
	DefaultToleranceBytes int64 = 1
)

var (
	ErrOverflow  = errors.New("fixed point overflow")
	ErrNegative  = errors.New("fixed point value must be non-negative")
	ErrTolerance = errors.New("fixed point tolerance exceeded")
)

// Result is a conversion intermediate suitable for diagnostics and truth
// comparison. It never hides saturation behind an ordinary value.
type Result struct {
	Input       int64 `json:"input"`
	Factor      int64 `json:"factor,omitempty"`
	Value       int64 `json:"value"`
	Remainder   int64 `json:"remainder,omitempty"`
	Overflow    bool  `json:"overflow"`
	Saturated   bool  `json:"saturated"`
	Tolerance   int64 `json:"tolerance_bytes,omitempty"`
	Diff        int64 `json:"diff_bytes,omitempty"`
	WithinBound bool  `json:"within_bound,omitempty"`
}

// CheckedMul multiplies two non-negative int64 values and reports overflow.
func CheckedMul(a, b int64) (int64, bool) {
	if a < 0 || b < 0 {
		return 0, true
	}
	if a == 0 || b == 0 {
		return 0, false
	}
	if a > math.MaxInt64/b {
		return 0, true
	}
	return a * b, false
}

// SaturatingMul returns math.MaxInt64 when the checked product overflows.
func SaturatingMul(a, b int64) Result {
	v, overflow := CheckedMul(a, b)
	if overflow {
		return Result{Input: a, Factor: b, Value: math.MaxInt64, Overflow: true, Saturated: true}
	}
	return Result{Input: a, Factor: b, Value: v}
}

// PermilleToPPM converts a per-mille numerator (scale 1,000) to PPM.
func PermilleToPPM(permille int64) Result {
	result := SaturatingMul(permille, Scale/1_000)
	result.Input = permille
	if !result.Overflow {
		result.Value = clampRate(result.Value)
	} else {
		result.Value = MaxRatePPM
	}
	return result
}

// PerMyriadToPPM converts a per-ten-thousand numerator (scale 10,000) to PPM.
func PerMyriadToPPM(perMyriad int64) Result {
	result := SaturatingMul(perMyriad, Scale/10_000)
	result.Input = perMyriad
	if !result.Overflow {
		result.Value = clampRate(result.Value)
	} else {
		result.Value = MaxRatePPM
	}
	return result
}

func clampRate(ppm int64) int64 {
	if ppm < 0 {
		return 0
	}
	if ppm > MaxRatePPM {
		return MaxRatePPM
	}
	return ppm
}

// KiBToBytes converts kibibytes to bytes using checked 64-bit arithmetic.
func KiBToBytes(kib int64) Result {
	result := SaturatingMul(kib, KiB)
	result.Input = kib
	if result.Overflow {
		result.Value = math.MaxInt64
	}
	return result
}

// BytesToKiBFloor converts bytes to whole KiB.
func BytesToKiBFloor(bytes int64) int64 {
	if bytes < 0 {
		return 0
	}
	return bytes / KiB
}

// RateBytes applies a PPM rate to a byte capacity using integer arithmetic.
// A big integer product is used only to detect whether the declared int64
// result width overflows; no floating point is involved.
func RateBytes(capacityBytes, ratePPM int64) Result {
	if capacityBytes < 0 || ratePPM < 0 {
		return Result{Input: capacityBytes, Factor: ratePPM, Value: 0, Overflow: true, Saturated: true}
	}
	product := new(big.Int).Mul(big.NewInt(capacityBytes), big.NewInt(ratePPM))
	quotient, remainder := new(big.Int).QuoRem(product, big.NewInt(Scale), new(big.Int))
	if !quotient.IsInt64() {
		return Result{Input: capacityBytes, Factor: ratePPM, Value: math.MaxInt64, Overflow: true, Saturated: true}
	}
	rem := int64(0)
	if remainder.IsInt64() {
		rem = remainder.Int64()
	}
	return Result{
		Input:     capacityBytes,
		Factor:    ratePPM,
		Value:     quotient.Int64(),
		Remainder: rem,
	}
}

// CheckedAdd adds non-negative bytes without silently wrapping.
func CheckedAdd(a, b int64) (int64, error) {
	if a < 0 || b < 0 {
		return 0, ErrNegative
	}
	if a > math.MaxInt64-b {
		return math.MaxInt64, ErrOverflow
	}
	return a + b, nil
}

// Diff returns a-b saturated to int64 range and the absolute difference.
func Diff(actual, expected int64) int64 {
	if actual >= expected {
		return actual - expected
	}
	return expected - actual
}

// WithinTolerance reports whether |actual-expected| <= tolerance.
func WithinTolerance(actual, expected, tolerance int64) bool {
	if tolerance < 0 {
		tolerance = DefaultToleranceBytes
	}
	return Diff(actual, expected) <= tolerance
}

// CheckBound annotates a conversion result against an expected truth value.
func CheckBound(result Result, expected, tolerance int64) Result {
	result.Tolerance = tolerance
	result.Diff = Diff(result.Value, expected)
	result.WithinBound = result.Diff <= tolerance && !result.Overflow
	return result
}
