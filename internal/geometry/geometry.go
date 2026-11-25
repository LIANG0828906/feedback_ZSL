package geometry

import (
	"math"

	"atelier/internal/model"
)

const eps = 1e-9

func Cross(a, b, c model.Point) float64 {
	return (b.X-a.X)*(c.Y-a.Y) - (b.Y-a.Y)*(c.X-a.X)
}

func OnSegment(a, b, p model.Point) bool {
	if math.Abs(Cross(a, b, p)) > 1e-8 {
		return false
	}
	return p.X >= math.Min(a.X, b.X)-eps && p.X <= math.Max(a.X, b.X)+eps &&
		p.Y >= math.Min(a.Y, b.Y)-eps && p.Y <= math.Max(a.Y, b.Y)+eps
}

func PointInPolygon(p model.Point, poly model.Polygon) (inside bool, boundary bool) {
	verts := poly.Vertices
	if len(verts) < 4 {
		return false, false
	}
	for i := 0; i < len(verts)-1; i++ {
		if OnSegment(verts[i], verts[i+1], p) {
			return false, true
		}
	}
	inside = false
	for i, j := 0, len(verts)-1; i < len(verts); j, i = i, i+1 {
		vi, vj := verts[i], verts[j]
		if (vi.Y > p.Y) != (vj.Y > p.Y) {
			x := (vj.X-vi.X)*(p.Y-vi.Y)/(vj.Y-vi.Y) + vi.X
			if p.X < x {
				inside = !inside
			}
		}
	}
	return inside, false
}

func StrictlyInside(p model.Point, poly model.Polygon) bool {
	inside, boundary := PointInPolygon(p, poly)
	return inside && !boundary
}

func SegmentIntersection(a, b, c, d model.Point) (model.Point, bool) {
	r := model.Point{X: b.X - a.X, Y: b.Y - a.Y}
	s := model.Point{X: d.X - c.X, Y: d.Y - c.Y}
	den := r.X*s.Y - r.Y*s.X
	if math.Abs(den) < 1e-10 {
		return model.Point{}, false
	}
	qp := model.Point{X: c.X - a.X, Y: c.Y - a.Y}
	t := (qp.X*s.Y - qp.Y*s.X) / den
	u := (qp.X*r.Y - qp.Y*r.X) / den
	if t >= -eps && t <= 1+eps && u >= -eps && u <= 1+eps {
		return model.Point{X: a.X + t*r.X, Y: a.Y + t*r.Y}, true
	}
	return model.Point{}, false
}

func ProperIntersection(a, b, c, d model.Point) bool {
	c1 := Cross(a, b, c)
	c2 := Cross(a, b, d)
	c3 := Cross(c, d, a)
	c4 := Cross(c, d, b)
	return ((c1 > eps && c2 < -eps) || (c1 < -eps && c2 > eps)) &&
		((c3 > eps && c4 < -eps) || (c3 < -eps && c4 > eps))
}

// FirstSelfIntersection returns the zero-based vertex index whose outgoing edge
// first intersects a non-adjacent edge.
func FirstSelfIntersection(poly model.Polygon) (int, bool) {
	v := poly.Vertices
	n := len(v) - 1
	if n < 3 {
		return 0, false
	}
	for i := 0; i < n; i++ {
		a, b := v[i], v[i+1]
		for j := i + 1; j < n; j++ {
			if j == i || j == i+1 {
				continue
			}
			if i == 0 && j == n-1 {
				continue
			}
			c, d := v[j], v[j+1]
			if ProperIntersection(a, b, c, d) {
				return i, true
			}
		}
	}
	return 0, false
}

func PolygonsOverlap(a, b model.Polygon) bool {
	ca, cb := a.Centroid(), b.Centroid()
	if StrictlyInside(ca, b) || StrictlyInside(cb, a) {
		return true
	}
	for _, p := range a.Vertices {
		if StrictlyInside(p, b) {
			return true
		}
	}
	for _, p := range b.Vertices {
		if StrictlyInside(p, a) {
			return true
		}
	}
	av, bv := a.Vertices, b.Vertices
	for i := 0; i < len(av)-1; i++ {
		for j := 0; j < len(bv)-1; j++ {
			if ProperIntersection(av[i], av[i+1], bv[j], bv[j+1]) {
				return true
			}
		}
	}
	return false
}

func PolygonInside(inner, outer model.Polygon) (int, bool) {
	for i, p := range inner.Vertices {
		inside, boundary := PointInPolygon(p, outer)
		if !inside && !boundary {
			return i, false
		}
	}
	for i := 0; i < len(inner.Vertices)-1; i++ {
		a, b := inner.Vertices[i], inner.Vertices[i+1]
		mid := model.Point{X: (a.X + b.X) / 2, Y: (a.Y + b.Y) / 2}
		inside, boundary := PointInPolygon(mid, outer)
		if !inside && !boundary {
			return i, false
		}
	}
	return -1, true
}

func PolygonsIntersect(a, b model.Polygon) bool {
	return PolygonsOverlap(a, b)
}
