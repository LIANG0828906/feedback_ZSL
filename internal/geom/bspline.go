package geom

import (
	"math"

	"cadlab/internal/model"
)

type SurfaceEval struct {
	Position  model.Vec3
	U         model.Vec3
	V         model.Vec3
	UU        model.Vec3
	UV        model.Vec3
	VV        model.Vec3
	Normal    model.Vec3
	Curvature float64
}

// Evaluate evaluates the rational B-spline surface and its first two
// parameter derivatives. All derivatives are analytical; no finite differences
// are used.
func Evaluate(p model.SurfacePatch, u, v float64) SurfaceEval {
	u0, u1, v0, v1 := p.Domain()
	u = clamp(u, u0, u1)
	v = clamp(v, v0, v1)
	base := ratGridFromPatch(p)
	du, okU := deriveURat(base)
	dv, okV := deriveVRat(base)
	var duu, dvv, duv ratGrid
	var okUU, okVV, okUV bool
	if okU {
		duu, okUU = deriveURat(du)
		duv, okUV = deriveVRat(du)
	}
	if okV {
		dvv, okVV = deriveVRat(dv)
	}
	a00, w00 := evalRatGrid(base, u, v)
	if math.Abs(w00) < 1e-15 {
		return SurfaceEval{Position: model.Vec3{}, Normal: model.Vec3{Y: 1}}
	}
	a10, w10 := evalOptionalRat(du, okU, u, v)
	a01, w01 := evalOptionalRat(dv, okV, u, v)
	a20, w20 := evalOptionalRat(duu, okUU, u, v)
	a02, w02 := evalOptionalRat(dvv, okVV, u, v)
	a11, w11 := evalOptionalRat(duv, okUV, u, v)
	s := a00.Mul(1 / w00)
	su := a10.Sub(s.Mul(w10)).Mul(1 / w00)
	sv := a01.Sub(s.Mul(w01)).Mul(1 / w00)
	suu := a20.Sub(su.Mul(2 * w10)).Sub(s.Mul(w20)).Mul(1 / w00)
	svv := a02.Sub(sv.Mul(2 * w01)).Sub(s.Mul(w02)).Mul(1 / w00)
	suv := a11.Sub(su.Mul(w01)).Sub(sv.Mul(w10)).Sub(s.Mul(w11)).Mul(1 / w00)
	normal := su.Cross(sv).Normalize()
	if normal == (model.Vec3{}) {
		normal = model.Vec3{Y: 1}
	}
	return SurfaceEval{
		Position: s, U: su, V: sv, UU: suu, UV: suv, VV: svv,
		Normal: normal, Curvature: curvature(su, sv, suu, suv, svv, normal),
	}
}

func curvature(su, sv, suu, suv, svv, normal model.Vec3) float64 {
	e, f, g := su.Dot(su), su.Dot(sv), sv.Dot(sv)
	l, m, n := normal.Dot(suu), normal.Dot(suv), normal.Dot(svv)
	den := e*g - f*f
	if math.Abs(den) < 1e-14 {
		return 0
	}
	gauss := (l*n - m*m) / den
	mean := (e*n - 2*f*m + g*l) / (2 * den)
	disc := mean*mean - gauss
	if disc < 0 {
		disc = 0
	}
	return math.Abs(mean) + math.Sqrt(disc)
}

func findSpan(n, p int, u float64, knots []float64) int {
	if u >= knots[n+1] {
		return n
	}
	if u <= knots[p] {
		return p
	}
	low, high := p, n+1
	mid := (low + high) / 2
	for u < knots[mid] || u >= knots[mid+1] {
		if u < knots[mid] {
			high = mid
		} else {
			low = mid
		}
		mid = (low + high) / 2
	}
	return mid
}

func clamp(v, lo, hi float64) float64 {
	if v < lo {
		return lo
	}
	if v > hi {
		return hi
	}
	return v
}

// ControlSupport returns the parameter rectangle influenced by a control point.
func ControlSupport(p model.SurfacePatch, row, col int) (u0, u1, v0, v1 float64) {
	du0, du1, dv0, dv1 := p.Domain()
	if col < 0 || col >= p.UCount || row < 0 || row >= p.VCount {
		return du0, du1, dv0, dv1
	}
	u0 = clamp(p.UKnots[col], du0, du1)
	u1 = clamp(p.UKnots[col+p.UDegree+1], du0, du1)
	v0 = clamp(p.VKnots[row], dv0, dv1)
	v1 = clamp(p.VKnots[row+p.VDegree+1], dv0, dv1)
	if u1 <= u0 {
		u1 = math.Nextafter(u0, math.Inf(1))
	}
	if v1 <= v0 {
		v1 = math.Nextafter(v0, math.Inf(1))
	}
	return
}
