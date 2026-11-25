// Package fixedpoint implements the deterministic Q20-ish geometry kernel.
//
// Scale is 1e6, so one fixed-point unit is one micro-metre. Samples and
// validation intentionally limit coordinates to +/-1000 metres, keeping
// squared distances inside int64 while retaining micrometre precision.
package fixedpoint

import (
	"math"
	"math/big"
)

const (
	Scale int64 = 1_000_000
	// MaxCoordinateM is the supported engineering coordinate bound.
	MaxCoordinateM = 1000.0
)

// V is a two-dimensional fixed-point vector.
type V struct {
	X int64 `json:"x_fixed"`
	Y int64 `json:"y_fixed"`
}

func FromMeters(x, y float64) V {
	return V{X: int64(math.Round(x * float64(Scale))), Y: int64(math.Round(y * float64(Scale)))}
}

func (v V) Meters() (float64, float64) {
	return float64(v.X) / float64(Scale), float64(v.Y) / float64(Scale)
}

func Add(a, b V) V { return V{X: a.X + b.X, Y: a.Y + b.Y} }
func Sub(a, b V) V { return V{X: a.X - b.X, Y: a.Y - b.Y} }

func Dot(a, b V) int64 {
	return a.X*b.X + a.Y*b.Y
}

func Cross(a, b V) int64 {
	return a.X*b.Y - a.Y*b.X
}

func AbsInt64(v int64) int64 {
	if v < 0 {
		return -v
	}
	return v
}

// SqDistance returns squared fixed-point distance. Callers must have
// validated the coordinate bound.
func SqDistance(a, b V) int64 {
	dx := a.X - b.X
	dy := a.Y - b.Y
	return dx*dx + dy*dy
}

// Distance returns the floor-rounded fixed-point Euclidean distance. The
// integer correction around math.Sqrt makes the result exact floor(sqrt(n)).
func Distance(a, b V) int64 {
	n := uint64(SqDistance(a, b))
	return int64(ISqrt(n))
}

// ISqrt returns floor(sqrt(n)) using integer correction.
func ISqrt(n uint64) uint64 {
	q := uint64(math.Sqrt(float64(n)))
	for (q+1)*(q+1) <= n {
		q++
	}
	for q*q > n {
		q--
	}
	return q
}

// Length returns fixed-point vector length.
func Length(v V) int64 {
	return int64(ISqrt(uint64(v.X*v.X + v.Y*v.Y)))
}

// UnitNormal returns a Q-Scale unit normal to d, choosing (-dy, dx).
func UnitNormal(d V) V {
	l := Length(d)
	if l == 0 {
		return V{}
	}
	return V{
		X: RoundedMulDiv(-d.Y, Scale, l),
		Y: RoundedMulDiv(d.X, Scale, l),
	}
}

// RoundedMulDiv computes round(v*mul/div) without float arithmetic.
func RoundedMulDiv(v, mul, div int64) int64 {
	if div == 0 {
		return 0
	}
	n := new(big.Int).Mul(big.NewInt(v), big.NewInt(mul))
	n.Quo(n, big.NewInt(div))
	return n.Int64()
}

// RoundRatToFixed converts a rational expressed in fixed-point coordinate
// units to the nearest fixed-point coordinate.
func RoundRatToFixed(x *big.Rat) int64 {
	q, r := new(big.Int).QuoRem(x.Num(), x.Denom(), new(big.Int))
	if r.Sign() == 0 {
		return q.Int64()
	}
	// QuoRem truncates toward zero. Round halves away from zero.
	twiceR := new(big.Int).Abs(r)
	twiceR.Mul(twiceR, big.NewInt(2))
	if twiceR.Cmp(x.Denom()) >= 0 {
		if x.Num().Sign() < 0 {
			q.Sub(q, big.NewInt(1))
		} else {
			q.Add(q, big.NewInt(1))
		}
	}
	return q.Int64()
}

// ClampInt64 restricts v to [lo, hi].
func ClampInt64(v, lo, hi int64) int64 {
	if v < lo {
		return lo
	}
	if v > hi {
		return hi
	}
	return v
}
