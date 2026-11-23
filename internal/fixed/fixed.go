// Package fixed implements the deterministic Q16.16 arithmetic used by the
// sample generator, path solver and reverberation estimates. All public
// calculations return integer fixed-point values so results never depend on
// platform floating-point formatting.
package fixed

import (
	"fmt"
	"math"
)

const (
	FractionBits = 16
	Scale        = int64(1) << FractionBits
)

// Value is a signed Q16.16 number.
type Value int64

func FromFloat(v float64) Value {
	return Value(math.Round(v * float64(Scale)))
}

func FromInt(v int64) Value { return Value(v * Scale) }

func (v Value) Float() float64 { return float64(v) / float64(Scale) }

func (v Value) Int64() int64 { return int64(v) }

func (v Value) String() string { return fmt.Sprintf("%.6f", v.Float()) }

func Add(a, b Value) Value { return a + b }
func Sub(a, b Value) Value { return a - b }

// Mul multiplies two fixed-point values and rounds toward zero.
func Mul(a, b Value) Value {
	return Value((int64(a) * int64(b)) / Scale)
}

// Div divides two fixed-point values and rounds toward zero.
func Div(a, b Value) Value {
	if b == 0 {
		return 0
	}
	return Value((int64(a) * Scale) / int64(b))
}

// MulInt multiplies a fixed-point value by an integer.
func MulInt(a Value, n int64) Value { return Value(int64(a) * n) }

// DivInt divides a fixed-point value by a positive integer.
func DivInt(a Value, n int64) Value {
	if n == 0 {
		return 0
	}
	return Value(int64(a) / n)
}

// MulDiv computes a*b/c in one integer expression. Callers use values small
// enough for int64; all level and acoustic values in this sample are bounded.
func MulDiv(a, b Value, c int64) Value {
	if c == 0 {
		return 0
	}
	return Value((int64(a) * int64(b)) / c)
}

// Sqrt returns the floor of sqrt(n). Newton iteration is fully deterministic.
func Sqrt(n uint64) uint64 {
	if n < 2 {
		return n
	}
	x := n
	y := (x + 1) / 2
	for y < x {
		x = y
		y = (x + n/x) / 2
	}
	return x
}

// Distance returns the Euclidean distance between two millimetre coordinates
// using fixed-point squares and an integer square root.
func Distance(ax, ay, bx, by float64) Value {
	dx := int64(FromFloat(ax) - FromFloat(bx))
	dy := int64(FromFloat(ay) - FromFloat(by))
	sum := uint64(dx*dx + dy*dy)
	return Value(int64(Sqrt(sum)))
}

func Clamp(v, lo, hi Value) Value {
	if v < lo {
		return lo
	}
	if v > hi {
		return hi
	}
	return v
}
