// Package geom contains small, dependency-free 2D geometry primitives.
package geom

import (
	"math"
	"sort"
)

const Epsilon = 1e-9

// Point is a two-dimensional point. Geometry uses float64 internally even
// though source glyph coordinates are quantized integers.
type Point struct {
	X float64 `json:"x"`
	Y float64 `json:"y"`
}

func (p Point) Add(q Point) Point        { return Point{p.X + q.X, p.Y + q.Y} }
func (p Point) Sub(q Point) Point        { return Point{p.X - q.X, p.Y - q.Y} }
func (p Point) Mul(s float64) Point      { return Point{p.X * s, p.Y * s} }
func (p Point) Dot(q Point) float64      { return p.X*q.X + p.Y*q.Y }
func (p Point) Cross(q Point) float64    { return p.X*q.Y - p.Y*q.X }
func (p Point) Len() float64             { return math.Hypot(p.X, p.Y) }
func (p Point) Distance(q Point) float64 { return p.Sub(q).Len() }

func (p Point) Normalize() Point {
	l := p.Len()
	if l < Epsilon {
		return Point{}
	}
	return p.Mul(1 / l)
}

func (p Point) PerpRight() Point { return Point{Y: -p.X, X: p.Y} }
func (p Point) PerpLeft() Point  { return Point{X: -p.Y, Y: p.X} }

// Bounds is an axis-aligned bounding box.
type Bounds struct {
	MinX float64 `json:"minX"`
	MinY float64 `json:"minY"`
	MaxX float64 `json:"maxX"`
	MaxY float64 `json:"maxY"`
}

func EmptyBounds() Bounds {
	return Bounds{MinX: math.Inf(1), MinY: math.Inf(1), MaxX: math.Inf(-1), MaxY: math.Inf(-1)}
}

func (b *Bounds) AddPoint(p Point) {
	if p.X < b.MinX {
		b.MinX = p.X
	}
	if p.Y < b.MinY {
		b.MinY = p.Y
	}
	if p.X > b.MaxX {
		b.MaxX = p.X
	}
	if p.Y > b.MaxY {
		b.MaxY = p.Y
	}
}

func (b Bounds) Valid() bool     { return b.MaxX >= b.MinX && b.MaxY >= b.MinY }
func (b Bounds) Width() float64  { return b.MaxX - b.MinX }
func (b Bounds) Height() float64 { return b.MaxY - b.MinY }
func (b Bounds) Expand(m float64) Bounds {
	return Bounds{MinX: b.MinX - m, MinY: b.MinY - m, MaxX: b.MaxX + m, MaxY: b.MaxY + m}
}
func (b Bounds) Contains(p Point) bool {
	return p.X >= b.MinX-Epsilon && p.X <= b.MaxX+Epsilon &&
		p.Y >= b.MinY-Epsilon && p.Y <= b.MaxY+Epsilon
}
func (b Bounds) ContainsBounds(o Bounds) bool {
	return b.MinX <= o.MinX+Epsilon && b.MinY <= o.MinY+Epsilon &&
		b.MaxX >= o.MaxX-Epsilon && b.MaxY >= o.MaxY-Epsilon
}
func (b Bounds) Intersects(o Bounds) bool {
	return b.MinX <= o.MaxX+Epsilon && b.MaxX >= o.MinX-Epsilon &&
		b.MinY <= o.MaxY+Epsilon && b.MaxY >= o.MinY-Epsilon
}
func (b Bounds) Center() Point {
	return Point{X: (b.MinX + b.MaxX) / 2, Y: (b.MinY + b.MaxY) / 2}
}

func BoundsOf(points []Point) Bounds {
	b := EmptyBounds()
	for _, p := range points {
		b.AddPoint(p)
	}
	if len(points) == 0 {
		return Bounds{}
	}
	return b
}

// SignedArea returns the shoelace area of a polygon. Positive means CCW.
func SignedArea(poly []Point) float64 {
	if len(poly) < 3 {
		return 0
	}
	var sum float64
	for i, p := range poly {
		q := poly[(i+1)%len(poly)]
		sum += p.Cross(q)
	}
	return sum / 2
}

func Area(poly []Point) float64 { return math.Abs(SignedArea(poly)) }

// FlattenQuad and FlattenCubic use a deterministic adaptive subdivision.
func FlattenQuad(p0, p1, p2 Point, tolerance float64) []Point {
	return flatten(p0, p2, func(t float64) Point {
		u := 1 - t
		return Point{
			X: u*u*p0.X + 2*u*t*p1.X + t*t*p2.X,
			Y: u*u*p0.Y + 2*u*t*p1.Y + t*t*p2.Y,
		}
	}, func() float64 {
		return pointLineDistance(p1, p0, p2)
	}, tolerance, 1)
}

func FlattenCubic(p0, p1, p2, p3 Point, tolerance float64) []Point {
	return flatten(p0, p3, func(t float64) Point {
		u := 1 - t
		return Point{
			X: u*u*u*p0.X + 3*u*u*t*p1.X + 3*u*t*t*p2.X + t*t*t*p3.X,
			Y: u*u*u*p0.Y + 3*u*u*t*p1.Y + 3*u*t*t*p2.Y + t*t*t*p3.Y,
		}
	}, func() float64 {
		return math.Max(pointLineDistance(p1, p0, p3), pointLineDistance(p2, p0, p3))
	}, tolerance, 1)
}

func flatten(p0, p1 Point, eval func(float64) Point, flatness func() float64, tol float64, depth int) []Point {
	if depth >= 20 || flatness() <= tol {
		return []Point{p1}
	}
	mid := eval(0.5)
	// Split using De Casteljau control points is not passed here, so use a
	// common recursive t split and accept the deterministic result.
	left := flattenRange(p0, mid, eval, 0, 0.5, tol, depth+1)
	right := flattenRange(mid, p1, eval, 0.5, 1, tol, depth+1)
	out := append(left, right...)
	return out
}

func flattenRange(start, end Point, eval func(float64) Point, t0, t1, tol float64, depth int) []Point {
	midT := (t0 + t1) / 2
	mid := eval(midT)
	if depth >= 20 || pointLineDistance(mid, start, end) <= tol {
		return []Point{end}
	}
	left := flattenRange(start, mid, eval, t0, midT, tol, depth+1)
	right := flattenRange(mid, end, eval, midT, t1, tol, depth+1)
	return append(left, right...)
}

func pointLineDistance(p, a, b Point) float64 {
	ab := b.Sub(a)
	l := ab.Len()
	if l < Epsilon {
		return p.Distance(a)
	}
	return math.Abs(ab.Cross(p.Sub(a))) / l
}

// SegmentIntersection returns true for proper or touching intersections.
// Shared endpoints are reported; polygon checks exclude adjacent edges.
func SegmentIntersection(a, b, c, d Point) (Point, bool) {
	r := b.Sub(a)
	s := d.Sub(c)
	den := r.Cross(s)
	qp := c.Sub(a)
	if math.Abs(den) < Epsilon {
		return Point{}, false
	}
	t := qp.Cross(s) / den
	u := qp.Cross(r) / den
	if t >= -Epsilon && t <= 1+Epsilon && u >= -Epsilon && u <= 1+Epsilon {
		return a.Add(r.Mul(t)), true
	}
	return Point{}, false
}

// PolygonSelfIntersects checks all non-adjacent edge pairs. A repeated closing
// point is accepted and ignored.
func PolygonSelfIntersects(poly []Point) (bool, Point) {
	pts := normalizePolygon(poly)
	n := len(pts)
	if n < 4 {
		return false, Point{}
	}
	for i := 0; i < n; i++ {
		a, b := pts[i], pts[(i+1)%n]
		for j := i + 1; j < n; j++ {
			if j == i || (j+1)%n == i || (i+1)%n == j {
				continue
			}
			c, d := pts[j], pts[(j+1)%n]
			if p, ok := segmentsIntersectInclusive(a, b, c, d); ok {
				return true, p
			}
		}
	}
	return false, Point{}
}

// PolylineSelfIntersects checks a non-closed path. Consecutive edges and the
// natural first/last gap are not treated as intersections.
func PolylineSelfIntersects(points []Point) (bool, Point) {
	n := len(points)
	if n < 4 {
		return false, Point{}
	}
	for i := 0; i < n-1; i++ {
		a, b := points[i], points[i+1]
		for j := i + 1; j < n-1; j++ {
			if j == i+1 {
				continue
			}
			c, d := points[j], points[j+1]
			if p, ok := segmentsIntersectInclusive(a, b, c, d); ok {
				return true, p
			}
		}
	}
	return false, Point{}
}

func segmentsIntersectInclusive(a, b, c, d Point) (Point, bool) {
	r := b.Sub(a)
	s := d.Sub(c)
	den := r.Cross(s)
	qp := c.Sub(a)
	if math.Abs(den) > Epsilon {
		t := qp.Cross(s) / den
		u := qp.Cross(r) / den
		if t >= -Epsilon && t <= 1+Epsilon && u >= -Epsilon && u <= 1+Epsilon {
			return a.Add(r.Mul(t)), true
		}
		return Point{}, false
	}
	if math.Abs(qp.Cross(r)) > Epsilon {
		return Point{}, false
	}
	if pointOnSegment(a, c, d) {
		return a, true
	}
	if pointOnSegment(b, c, d) {
		return b, true
	}
	if pointOnSegment(c, a, b) {
		return c, true
	}
	if pointOnSegment(d, a, b) {
		return d, true
	}
	return Point{}, false
}

func pointOnSegment(p, a, b Point) bool {
	ab := b.Sub(a)
	if math.Abs(ab.Cross(p.Sub(a))) > Epsilon*math.Max(1, ab.Len()) {
		return false
	}
	return p.X >= math.Min(a.X, b.X)-Epsilon && p.X <= math.Max(a.X, b.X)+Epsilon &&
		p.Y >= math.Min(a.Y, b.Y)-Epsilon && p.Y <= math.Max(a.Y, b.Y)+Epsilon
}

func normalizePolygon(poly []Point) []Point {
	if len(poly) > 1 && almostPoint(poly[0], poly[len(poly)-1]) {
		return poly[:len(poly)-1]
	}
	return poly
}

func almostPoint(a, b Point) bool {
	return math.Abs(a.X-b.X) < 1e-7 && math.Abs(a.Y-b.Y) < 1e-7
}

// ResamplePolyline resamples an open polyline by arc length. Endpoints are
// always included.
func ResamplePolyline(points []Point, step float64) []Point {
	if len(points) == 0 {
		return nil
	}
	if len(points) == 1 || step <= Epsilon {
		return append([]Point(nil), points...)
	}
	lengths := make([]float64, len(points))
	for i := 1; i < len(points); i++ {
		lengths[i] = lengths[i-1] + points[i].Distance(points[i-1])
	}
	total := lengths[len(lengths)-1]
	if total <= Epsilon {
		return []Point{points[0]}
	}
	count := int(math.Ceil(total / step))
	if count < 1 {
		count = 1
	}
	out := make([]Point, 0, count+1)
	out = append(out, points[0])
	seg := 1
	for k := 1; k <= count; k++ {
		target := math.Min(total, float64(k)*total/float64(count))
		for seg < len(points)-1 && lengths[seg] < target {
			seg++
		}
		segLen := lengths[seg] - lengths[seg-1]
		u := 0.0
		if segLen > Epsilon {
			u = (target - lengths[seg-1]) / segLen
		}
		out = append(out, points[seg-1].Add(points[seg].Sub(points[seg-1]).Mul(u)))
	}
	out[len(out)-1] = points[len(points)-1]
	return out
}

// PolylineLength returns the cumulative length of consecutive segments.
func PolylineLength(points []Point) float64 {
	var total float64
	for i := 1; i < len(points); i++ {
		total += points[i].Distance(points[i-1])
	}
	return total
}

// PointAtArcLength returns a point and local tangent on a polyline.
func PointAtArcLength(points []Point, s float64) (Point, Point) {
	if len(points) < 2 {
		if len(points) == 1 {
			return points[0], Point{X: 1}
		}
		return Point{}, Point{X: 1}
	}
	if s <= 0 {
		return points[0], points[1].Sub(points[0]).Normalize()
	}
	var acc float64
	for i := 1; i < len(points); i++ {
		d := points[i].Distance(points[i-1])
		if acc+d >= s || i == len(points)-1 {
			u := 0.0
			if d > Epsilon {
				u = (s - acc) / d
			}
			u = math.Max(0, math.Min(1, u))
			return points[i-1].Add(points[i].Sub(points[i-1]).Mul(u)), points[i].Sub(points[i-1]).Normalize()
		}
		acc += d
	}
	return points[len(points)-1], points[len(points)-1].Sub(points[len(points)-2]).Normalize()
}

// SortPointsByAngle sorts points around center. Useful for deterministic
// polygon diagnostics.
func SortPointsByAngle(points []Point, center Point) []Point {
	out := append([]Point(nil), points...)
	sort.SliceStable(out, func(i, j int) bool {
		ai := math.Atan2(out[i].Y-center.Y, out[i].X-center.X)
		aj := math.Atan2(out[j].Y-center.Y, out[j].X-center.X)
		return ai < aj
	})
	return out
}
