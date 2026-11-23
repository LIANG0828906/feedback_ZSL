package fixed

import (
	"math"
	"math/big"

	"inputkernel/internal/model"
)

// DivRound rounds num/den to the nearest integer, with exact ties away from zero.
func DivRound(num, den int64) int64 {
	if den == 0 {
		return 0
	}
	if den < 0 {
		num = -num
		den = -den
	}
	q := num / den
	r := num % den
	if r < 0 {
		r = -r
	}
	if r*2 >= den {
		if num >= 0 {
			q++
		} else {
			q--
		}
	}
	return q
}

// Normalize converts a raw axis value into fixed-point normalized units and
// returns the exact rounding residual before inversion.
func Normalize(a model.AxisSpec, scale, raw int64) (value int64, residual model.Fraction) {
	span := a.Max - a.Min
	if span <= 0 {
		return 0, model.Fraction{Num: 0, Den: 1}
	}
	var num int64
	if a.Bipolar {
		num = 2*(raw-a.Min)*scale - span*scale
	} else {
		num = (raw - a.Min) * scale
		if a.Direction == "negative" {
			num -= span * scale
		}
	}
	value = DivRound(num, span)
	return value, model.Fraction{Num: num - value*span, Den: span}
}

// NormalizeAxis applies the required pipeline: quantization, deadzone
// elimination, then axis inversion. The returned exact residual describes
// quantization error, not the intentional deadzone suppression.
func NormalizeAxis(a model.AxisSpec, scale, raw int64) (value int64, residual model.Fraction, inside bool, margin int64) {
	value, residual = Normalize(a, scale, raw)
	margin = absI64(absI64(value) - a.Deadzone)
	if absI64(value) <= a.Deadzone {
		value = 0
		inside = true
	}
	if a.Invert {
		value = -value
		residual.Num = -residual.Num
	}
	return value, residual, inside, margin
}

func absI64(v int64) int64 {
	if v < 0 {
		if v == math.MinInt64 {
			return math.MaxInt64
		}
		return -v
	}
	return v
}

func Clamp(v, lo, hi int64) int64 {
	if v < lo {
		return lo
	}
	if v > hi {
		return hi
	}
	return v
}

// EvalCurve evaluates a piecewise-linear curve with integer interpolation.
func EvalCurve(c model.Curve, x int64) int64 {
	if len(c.Points) == 0 {
		return 0
	}
	x = Clamp(x, -c.DomainScale, c.DomainScale)
	if x <= c.Points[0].X {
		return c.Points[0].Y
	}
	for i := 1; i < len(c.Points); i++ {
		p0, p1 := c.Points[i-1], c.Points[i]
		if x <= p1.X {
			dx := p1.X - p0.X
			if dx == 0 {
				return p1.Y
			}
			return p0.Y + DivRound((x-p0.X)*(p1.Y-p0.Y), dx)
		}
	}
	return c.Points[len(c.Points)-1].Y
}

// CurveLinearizationBound estimates the chord error over the segment
// containing x. It uses only the declared curvature bound and segment width.
func CurveLinearizationBound(c model.Curve, x int64) int64 {
	if c.CurvaturePPM <= 0 || len(c.Points) < 2 || c.DomainScale <= 0 {
		return 0
	}
	x = Clamp(x, -c.DomainScale, c.DomainScale)
	width := c.Points[len(c.Points)-1].X - c.Points[0].X
	for i := 1; i < len(c.Points); i++ {
		if x <= c.Points[i].X {
			width = c.Points[i].X - c.Points[i-1].X
			break
		}
	}
	if width <= 0 {
		return 0
	}
	// ceil(curvaturePPM * width^2 / (8 * domainScale * 1e6))
	num := new(big.Int).Mul(big.NewInt(c.CurvaturePPM), big.NewInt(width))
	num.Mul(num, big.NewInt(width))
	den := new(big.Int).Mul(big.NewInt(8*c.DomainScale), big.NewInt(1_000_000))
	quo, rem := new(big.Int).QuoRem(num, den, new(big.Int))
	if rem.Sign() > 0 {
		quo.Add(quo, big.NewInt(1))
	}
	if !quo.IsInt64() {
		return math.MaxInt64
	}
	return quo.Int64()
}

func Abs(v int64) int64 { return absI64(v) }

func FractionAbs(f model.Fraction) model.Fraction {
	if f.Num < 0 {
		f.Num = -f.Num
	}
	if f.Den < 0 {
		f.Num = -f.Num
		f.Den = -f.Den
	}
	return f
}
