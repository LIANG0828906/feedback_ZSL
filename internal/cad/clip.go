package cad

import (
	"fmt"
	"math"
)

const clipEps = 1e-10

func ValidateClip(patchID string, clip ClipBoundary) error {
	if len(clip.Loops) == 0 {
		return NewError(CodeClip, "clip boundary is empty").WithPatch(patchID).
			WithPath("$.clip.loops")
	}
	for loopIndex, loop := range clip.Loops {
		path := fmt.Sprintf("$.clip.loops[%d]", loopIndex)
		if len(loop) < 3 {
			return NewError(CodeClip, "clip loop requires at least 3 points").
				WithPatch(patchID).WithPath(path)
		}
		for i, p := range loop {
			if p.U < -clipEps || p.U > 1+clipEps || p.V < -clipEps || p.V > 1+clipEps ||
				math.IsNaN(p.U) || math.IsNaN(p.V) || math.IsInf(p.U, 0) || math.IsInf(p.V, 0) {
				conflict := p
				conflict.U = math.Max(0, math.Min(1, conflict.U))
				conflict.V = math.Max(0, math.Min(1, conflict.V))
				return NewError(CodeClip, "clip parameter point outside [0,1]").
					WithPatch(patchID).WithPath(fmt.Sprintf("%s[%d]", path, i)).WithPoint(p)
			}
		}
		if p, ok := firstSelfIntersection(loop); ok {
			return NewError(CodeClip, "clip boundary self-intersects").
				WithPatch(patchID).WithPath(path).WithPoint(p)
		}
	}
	return nil
}

func PointInsideClip(clip ClipBoundary, p Vec2) bool {
	inside := false
	for _, loop := range clip.Loops {
		if pointOnPolygonBoundary(loop, p) {
			return true
		}
		if pointInPolygon(loop, p) {
			inside = !inside
		}
	}
	return inside
}

func pointInPolygon(poly []Vec2, p Vec2) bool {
	inside := false
	n := len(poly)
	for i := 0; i < n; i++ {
		j := (i + n - 1) % n
		a, b := poly[j], poly[i]
		if (a.V > p.V) != (b.V > p.V) {
			x := a.U + (p.V-a.V)*(b.U-a.U)/(b.V-a.V)
			if x > p.U {
				inside = !inside
			}
		}
	}
	return inside
}

func pointOnPolygonBoundary(poly []Vec2, p Vec2) bool {
	for i := 0; i < len(poly); i++ {
		a := poly[i]
		b := poly[(i+1)%len(poly)]
		if distanceToSegment(p, a, b) <= 1e-9 {
			return true
		}
	}
	return false
}

func distanceToSegment(p, a, b Vec2) float64 {
	vx, vy := b.U-a.U, b.V-a.V
	wx, wy := p.U-a.U, p.V-a.V
	l2 := vx*vx + vy*vy
	if l2 == 0 {
		return math.Hypot(p.U-a.U, p.V-a.V)
	}
	t := math.Max(0, math.Min(1, (wx*vx+wy*vy)/l2))
	x := a.U + t*vx
	y := a.V + t*vy
	return math.Hypot(p.U-x, p.V-y)
}

func firstSelfIntersection(poly []Vec2) (Vec2, bool) {
	n := len(poly)
	for i := 0; i < n; i++ {
		a := poly[i]
		b := poly[(i+1)%n]
		for j := i + 1; j < n; j++ {
			if j == i || (i == 0 && j == n-1) {
				continue
			}
			c := poly[j]
			d := poly[(j+1)%n]
			if j == i+1 {
				c = poly[j]
				d = poly[(j+1)%n]
				if segmentsShareEndpoint(a, b, c, d) {
					continue
				}
			}
			if j == n-1 && i == 0 {
				continue
			}
			if p, hit := segmentIntersection(a, b, c, d); hit {
				return p, true
			}
		}
	}
	return Vec2{}, false
}

func segmentsShareEndpoint(a, b, c, d Vec2) bool {
	const eps = 1e-10
	same := func(x, y Vec2) bool { return math.Abs(x.U-y.U) < eps && math.Abs(x.V-y.V) < eps }
	return same(b, c) || same(a, d) || same(a, c) || same(b, d)
}

func segmentIntersection(a, b, c, d Vec2) (Vec2, bool) {
	r := Vec2{U: b.U - a.U, V: b.V - a.V}
	s := Vec2{U: d.U - c.U, V: d.V - c.V}
	den := cross2(r, s)
	qp := Vec2{U: c.U - a.U, V: c.V - a.V}
	if math.Abs(den) < 1e-12 {
		if math.Abs(cross2(qp, r)) > 1e-10 {
			return Vec2{}, false
		}
		t0 := dot2(qp, r) / dot2(r, r)
		t1 := t0 + dot2(s, r)/dot2(r, r)
		if math.Max(t0, t1) < 0 || math.Min(t0, t1) > 1 {
			return Vec2{}, false
		}
		t := math.Max(0, math.Min(1, t0))
		return Vec2{U: a.U + t*r.U, V: a.V + t*r.V}, true
	}
	t := cross2(qp, s) / den
	u := cross2(qp, r) / den
	if t < -1e-10 || t > 1+1e-10 || u < -1e-10 || u > 1+1e-10 {
		return Vec2{}, false
	}
	return Vec2{U: a.U + t*r.U, V: a.V + t*r.V}, true
}

func cross2(a, b Vec2) float64 { return a.U*b.V - a.V*b.U }
func dot2(a, b Vec2) float64   { return a.U*b.U + a.V*b.V }
