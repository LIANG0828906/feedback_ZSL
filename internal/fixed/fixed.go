// Package fixed implements the declared-precision fixed point arithmetic used
// by every metric. All intermediate accumulators are signed 64-bit Q16.16
// values. Every operation that discards fractional bits records a truncation
// event with the symbolic location of the value, so the first truncation in a
// chain is observable and independently reproducible.
package fixed

import (
	"math/bits"
)

// Scale is the number of fractional bits in Q16.16.
const Scale = 16

// One is the fixed point representation of 1.0.
const One = 1 << Scale

// Q is a signed Q16.16 fixed point value.
type Q int64

// Truncation records one place where fractional bits were discarded.
type Truncation struct {
	Location string `json:"location"`
	Input    int64  `json:"input"`
	Lost     int64  `json:"lost_lsb"`
}

// Tracer collects truncation events in occurrence order. A nil Tracer is valid
// and simply discards events.
type Tracer struct {
	Events []Truncation
}

// NewTracer creates an empty tracer.
func NewTracer() *Tracer { return &Tracer{} }

// FromInt converts an integer to fixed point without truncation.
func FromInt(v int64) Q { return Q(v << Scale) }

// FromInt32 converts an int32 to fixed point.
func FromInt32(v int32) Q { return Q(int64(v) << Scale) }

// Raw wraps a raw scaled integer.
func Raw(v int64) Q { return Q(v) }

// Int returns the integer part truncating towards zero.
func (q Q) Int() int64 { return int64(q) >> Scale }

// TruncatedInt returns the integer part and records the discarded low bits at
// location when a tracer is supplied.
func (q Q) TruncatedInt(location string, t *Tracer) int64 {
	raw := int64(q)
	lost := raw & (One - 1)
	if lost != 0 && t != nil {
		t.Events = append(t.Events, Truncation{Location: location, Input: raw, Lost: lost})
	}
	return raw >> Scale
}

// Add and Sub cannot truncate; callers must keep accumulators in range.
func Add(a, b Q) Q { return Q(int64(a) + int64(b)) }

// Sub subtracts b from a.
func Sub(a, b Q) Q { return Q(int64(a) - int64(b)) }

// Mul multiplies two Q16.16 values. The signed 128-bit product is shifted
// right by Scale; discarded fractional bits are reported to the tracer.
// Truncation is always toward zero so negative products stay symmetric.
func Mul(a, b Q, location string, t *Tracer) Q {
	ai, bi := int64(a), int64(b)
	neg := (ai < 0) != (bi < 0)
	au, bu := uint64(ai), uint64(bi)
	if ai < 0 {
		au = uint64(-ai)
	}
	if bi < 0 {
		bu = uint64(-bi)
	}
	hi, lo := bits.Mul64(au, bu)
	if hi>>(64-Scale) != 0 {
		panic("fixed: multiply overflow")
	}
	lost := lo & (One - 1)
	if lost != 0 && t != nil {
		t.Events = append(t.Events, Truncation{Location: location, Input: int64(ai * bi), Lost: int64(lost)})
	}
	// Unsigned floor of (product >> Scale).
	out := int64((hi << (64 - Scale)) | (lo >> Scale))
	if neg {
		if lost != 0 {
			out++
		}
		out = -out
	}
	return Q(out)
}

// Div divides a by b. In Q16.16, a/b = (a_raw << Scale) / b_raw; the 128-bit
// numerator keeps the full range and the remainder is traced.
func Div(a, b Q, location string, t *Tracer) Q {
	if b == 0 {
		panic("fixed: divide by zero")
	}
	num := int64(a)
	den := int64(b)
	neg := (num < 0) != (den < 0)
	if num < 0 {
		num = -num
	}
	if den < 0 {
		den = -den
	}
	// Split (num << Scale) into (hi, lo); num is non-negative here.
	hi := uint64(num) >> (64 - Scale)
	lo := uint64(num) << Scale
	q64, rem := bits.Div64(uint64(hi), uint64(lo), uint64(den))
	if rem != 0 && t != nil {
		t.Events = append(t.Events, Truncation{Location: location, Input: num, Lost: int64(rem)})
	}
	if neg {
		q64 = -q64
		if rem != 0 {
			q64++ // truncate toward zero
		}
	}
	return Q(int64(q64))
}

// Sqrt computes a fixed point square root in Q16.16. It first finds the
// integer root with 64-bit Newton iterations, then runs Q16 refinement steps;
// no floating point is used and the result is bit-for-bit deterministic.
func Sqrt(a Q, location string, t *Tracer) Q {
	if a < 0 {
		panic("fixed: sqrt of negative")
	}
	if a == 0 {
		return 0
	}
	ai := uint64(int64(a))
	// Integer sqrt seed from the bit length, refined in plain integer units.
	y := uint64(1) << ((bits.Len64(ai) + 1) / 2)
	for i := 0; i < 32; i++ {
		next := (y + ai/y) >> 1
		if next == 0 || next >= y {
			break
		}
		y = next
	}
	// Lift into Q16 and refine the fractional part with fixed point Newton.
	x := y << Scale
	for i := 0; i < 40; i++ {
		num := ai * One
		q := num / x
		next := (x + q) >> 1
		if next == 0 || next >= x {
			break
		}
		x = next
	}
	if rem := (ai * One) % x; rem != 0 && t != nil {
		t.Events = append(t.Events, Truncation{Location: location, Input: int64(ai), Lost: int64(rem)})
	}
	return Q(int64(x))
}

// Float converts a fixed point value to float64 for final transcendental
// operations (log10) that have no fixed point equivalent.
func (q Q) Float() float64 { return float64(int64(q)) / float64(One) }
