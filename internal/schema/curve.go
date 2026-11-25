package schema

import (
	"terrainkernel/internal/fixed"
	"terrainkernel/internal/model"
)

// EvalCurve evaluates a non-decreasing piecewise-linear curve with integer
// interpolation. Values outside the control-point domain are clamped.
func EvalCurve(curve *model.Curve, x int64) int64 {
	if curve == nil || len(curve.Points) == 0 {
		return 0
	}
	pts := curve.Points
	if x <= pts[0].X {
		return pts[0].Y
	}
	if x >= pts[len(pts)-1].X {
		return pts[len(pts)-1].Y
	}
	lo, hi := 0, len(pts)-1
	for lo+1 < hi {
		mid := (lo + hi) / 2
		if pts[mid].X <= x {
			lo = mid
		} else {
			hi = mid
		}
	}
	a, b := pts[lo], pts[hi]
	span := b.X - a.X
	if span <= 0 {
		return b.Y
	}
	delta := b.Y - a.Y
	v, err := fixed.MulDivRounded(delta, x-a.X, span, "curve", fixed.Nearest)
	if err != nil {
		return b.Y
	}
	y, err := fixed.Add(a.Y, v, "curve")
	if err != nil {
		return b.Y
	}
	return y
}
