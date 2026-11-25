package fixedpoint

import (
	"math"
	"math/big"

	"sre-timeline/internal/bizerr"
)

const (
	Scale     int64 = 1_000_000
	WidthBits       = 64
)

var scaleBig = big.NewInt(Scale)

type Value struct {
	Raw       int64 `json:"raw"`
	Saturated bool  `json:"saturated,omitempty"`
}

func FromRatio(numerator, denominator int64) (Value, error) {
	if denominator == 0 {
		return Value{}, bizerr.New(bizerr.CodeFixedOverflow, "fixedpoint.denominator", "division by zero")
	}
	q := new(big.Int).Mul(big.NewInt(numerator), scaleBig)
	q.Quo(q, big.NewInt(denominator))
	if !q.IsInt64() {
		return Value{Raw: saturatingBig(q), Saturated: true}, bizerr.New(bizerr.CodeFixedOverflow, "fixedpoint.from_ratio", "ratio result exceeds int64")
	}
	return Value{Raw: q.Int64()}, nil
}

func FromBP(bp int) Value {
	return Value{Raw: int64(bp) * Scale / 10000}
}

func Check(raw int64) (Value, error) {
	return Value{Raw: raw}, nil
}

func Add(a, b Value) (Value, error) {
	if (b.Raw > 0 && a.Raw > math.MaxInt64-b.Raw) || (b.Raw < 0 && a.Raw < math.MinInt64-b.Raw) {
		return Value{Raw: clampAdd(a.Raw, b.Raw), Saturated: true},
			bizerr.New(bizerr.CodeFixedOverflow, "fixedpoint.add", "overflow adding fixed values").
				With("left", a.Raw).With("right", b.Raw)
	}
	return Value{Raw: a.Raw + b.Raw, Saturated: a.Saturated || b.Saturated}, nil
}

func AddSaturating(a, b Value) Value {
	out, err := Add(a, b)
	if err != nil {
		return Value{Raw: clampAdd(a.Raw, b.Raw), Saturated: true}
	}
	return out
}

// Mul multiplies two Scale-scaled fixed values. big.Int is used only to prove
// that the checked integer result fits in the declared 64-bit width.
func Mul(a, b Value) (Value, error) {
	raw, ok := mulChecked(a.Raw, b.Raw)
	if !ok {
		return Value{Raw: saturatingSign(a.Raw, b.Raw), Saturated: true},
			bizerr.New(bizerr.CodeFixedOverflow, "fixedpoint.mul", "overflow multiplying fixed values").
				With("left", a.Raw).With("right", b.Raw)
	}
	return Value{Raw: raw, Saturated: a.Saturated || b.Saturated}, nil
}

func MulSaturating(a, b Value) Value {
	out, err := Mul(a, b)
	if err != nil {
		return Value{Raw: saturatingSign(a.Raw, b.Raw), Saturated: true}
	}
	return out
}

func mulChecked(a, b int64) (int64, bool) {
	product := new(big.Int).Mul(big.NewInt(a), big.NewInt(b))
	product.Quo(product, scaleBig)
	if !product.IsInt64() {
		return 0, false
	}
	return product.Int64(), true
}

func saturatingBig(v *big.Int) int64 {
	if v.Sign() < 0 {
		return math.MinInt64
	}
	return math.MaxInt64
}

func saturatingSign(a, b int64) int64 {
	if (a < 0) != (b < 0) {
		return math.MinInt64
	}
	return math.MaxInt64
}

func clampAdd(a, b int64) int64 {
	if b > 0 && a > math.MaxInt64-b {
		return math.MaxInt64
	}
	if b < 0 && a < math.MinInt64-b {
		return math.MinInt64
	}
	return a + b
}

// ToScale converts raw Scale=1e6 fixed point to a coarser declaration scale.
func (v Value) ToScale(declaredScale int64) int64 {
	if declaredScale <= 0 {
		return 0
	}
	return v.Raw * declaredScale / Scale
}

// WithinTolerance reports whether two raw scores differ by at most tol.
func WithinTolerance(got, want, tol int64) bool {
	d := got - want
	if d < 0 {
		d = -d
	}
	return d <= tol
}
