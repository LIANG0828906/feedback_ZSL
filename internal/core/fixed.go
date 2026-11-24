package core

import "math"

const (
	// AmountScale is the fixed-point denominator. Every rate, stock and
	// capacity value is an integer number of thousandths of one resource unit.
	AmountScale int64 = 1000

	// MaxFixedValue keeps additions and per-tick multiplication comfortably
	// inside int64 even for very large generated worlds.
	MaxFixedValue int64 = 1 << 52
)

func ValidFixed(v int64) bool {
	return v >= 0 && v <= MaxFixedValue
}

func SafeAddNonNeg(a, b int64) (int64, bool) {
	if a < 0 || b < 0 || a > math.MaxInt64-b {
		return 0, false
	}
	return a + b, true
}

func MinInt64(a, b int64) int64 {
	if a < b {
		return a
	}
	return b
}

func MaxInt64(a, b int64) int64 {
	if a > b {
		return a
	}
	return b
}
