package engine

import (
	"math"

	"strokeforge/internal/model"
)

const epsilon = 1e-9

func dist(a, b model.FPoint) float64 {
	dx := a.X - b.X
	dy := a.Y - b.Y
	return math.Hypot(dx, dy)
}

// quadFlatness is the maximum perpendicular deviation of the control point
// from the chord, which bounds the curve-to-chord distance.
func quadFlatness(p0, c, p1 model.FPoint) float64 {
	return pointLineDistance(c, p0, p1)
}

func cubicFlatness(p0, c1, c2, p1 model.FPoint) float64 {
	return math.Max(pointLineDistance(c1, p0, p1), pointLineDistance(c2, p0, p1))
}

func pointLineDistance(p, a, b model.FPoint) float64 {
	dx := b.X - a.X
	dy := b.Y - a.Y
	len := math.Hypot(dx, dy)
	if len < epsilon {
		return dist(p, a)
	}
	return math.Abs(dy*p.X-dx*p.Y+b.X*a.Y-b.Y*a.X) / len
}

func toFP(p model.Point) model.FPoint {
	return model.FPoint{X: float64(p.X), Y: float64(p.Y)}
}

// flattenQuadratic recursively subdivides until the chord approximation is
// within tolerance. Output excludes the starting point of the span.
func flattenQuadratic(p0, c, p1 model.FPoint, tol float64, out []model.FPoint) []model.FPoint {
	if quadFlatness(p0, c, p1) <= tol {
		return append(out, p1)
	}
	p01 := midpoint(p0, c)
	p12 := midpoint(c, p1)
	p012 := midpoint(p01, p12)
	out = flattenQuadratic(p0, p01, p012, tol, out)
	out = flattenQuadratic(p012, p12, p1, tol, out)
	return out
}

// flattenCubic recursively subdivides a cubic span.
func flattenCubic(p0, c1, c2, p1 model.FPoint, tol float64, depth int, out []model.FPoint) []model.FPoint {
	if depth <= 0 || cubicFlatness(p0, c1, c2, p1) <= tol {
		return append(out, p1)
	}
	// de Casteljau split at t=0.5.
	p01 := midpoint(p0, c1)
	p12 := midpoint(c1, c2)
	p23 := midpoint(c2, p1)
	p012 := midpoint(p01, p12)
	p123 := midpoint(p12, p23)
	p0123 := midpoint(p012, p123)
	out = flattenCubic(p0, p01, p012, p0123, tol, depth-1, out)
	out = flattenCubic(p0123, p123, p23, p1, tol, depth-1, out)
	return out
}

func midpoint(a, b model.FPoint) model.FPoint {
	return model.FPoint{X: (a.X + b.X) / 2, Y: (a.Y + b.Y) / 2}
}

// FlattenContour turns a closed Bezier contour into a closed polyline. The
// final point is not repeated; closure is implicit between last and first.
func FlattenContour(c model.Contour, tol float64) ([]model.FPoint, error) {
	if len(c.Segments) == 0 {
		return nil, model.NewError(model.EDecode, "contour has no segments")
	}
	start := toFP(c.Segments[0].P0)
	out := []model.FPoint{start}
	cur := start
	for _, seg := range c.Segments {
		p0 := toFP(seg.P0)
		if dist(p0, cur) > epsilon {
			return nil, model.NewError(model.EDecode, "segment chain is not continuous").
				WithField("segment_id")
		}
		switch seg.Typ {
		case model.SegQuadratic:
			cp := toFP(seg.C1)
			p1 := toFP(seg.P1)
			if dist(p0, p1) < epsilon && dist(cp, p0) < epsilon {
				out = append(out, p1)
			} else {
				out = flattenQuadratic(p0, cp, p1, tol, out)
			}
			cur = p1
		case model.SegCubic:
			c1 := toFP(seg.C1)
			c2 := toFP(seg.C2)
			p1 := toFP(seg.P1)
			if dist(p0, p1) < epsilon && dist(c1, p0) < epsilon && dist(c2, p0) < epsilon {
				out = append(out, p1)
			} else {
				out = flattenCubic(p0, c1, c2, p1, tol, 24, out)
			}
			cur = p1
		default:
			return nil, model.NewError(model.EDecode, "unsupported segment type: "+seg.Typ).
				WithField("type")
		}
	}
	// Drop a duplicated closing point.
	if len(out) > 1 && dist(out[0], out[len(out)-1]) < epsilon {
		out = out[:len(out)-1]
	}
	return out, nil
}

// ArcTable holds cumulative arc length at every polyline vertex.
type ArcTable struct {
	Pts    []model.FPoint
	Cum    []float64
	Length float64
}

// BuildArcTable constructs the cumulative table for a closed polyline.
func BuildArcTable(pts []model.FPoint) ArcTable {
	cum := make([]float64, len(pts)+1)
	var total float64
	for i := range pts {
		total += dist(pts[i], pts[(i+1)%len(pts)])
		cum[i+1] = total
	}
	return ArcTable{Pts: pts, Cum: cum, Length: total}
}

// AtRatio returns the point located at ratio r in [0,1] of the closed arc.
func (t ArcTable) AtRatio(r float64) model.FPoint {
	if r <= 0 {
		return t.Pts[0]
	}
	if r >= 1 {
		return t.Pts[0]
	}
	target := r * t.Length
	// Binary search the edge whose cumulative span contains target.
	lo, hi := 0, len(t.Cum)-1
	for lo+1 < hi {
		mid := (lo + hi) / 2
		if t.Cum[mid] <= target {
			lo = mid
		} else {
			hi = mid
		}
	}
	edgeLen := t.Cum[lo+1] - t.Cum[lo]
	if edgeLen < epsilon {
		return t.Pts[lo%len(t.Pts)]
	}
	f := (target - t.Cum[lo]) / edgeLen
	a := t.Pts[lo%len(t.Pts)]
	b := t.Pts[(lo+1)%len(t.Pts)]
	return model.FPoint{X: a.X + (b.X-a.X)*f, Y: a.Y + (b.Y-a.Y)*f}
}

// Slice returns the open polyline portion covering arc ratios [r0,r1].
// Endpoints are always included; the result has at least two points.
func (t ArcTable) Slice(r0, r1 float64) []model.FPoint {
	start := t.AtRatio(r0)
	end := t.AtRatio(r1)
	out := []model.FPoint{start}
	d0 := r0 * t.Length
	d1 := r1 * t.Length
	for i := range t.Pts {
		d := t.Cum[i+1]
		if d > d0+epsilon && d < d1-epsilon {
			out = append(out, t.Pts[(i+1)%len(t.Pts)])
		}
	}
	out = append(out, end)
	// De-duplicate consecutive identical points.
	clean := out[:0]
	for _, p := range out {
		if len(clean) == 0 || dist(clean[len(clean)-1], p) > epsilon {
			clean = append(clean, p)
		}
	}
	return clean
}

// SignedArea applies the shoelace formula to a closed polyline.
func SignedArea(pts []model.FPoint) float64 {
	var twice float64
	for i := range pts {
		a := pts[i]
		b := pts[(i+1)%len(pts)]
		twice += a.X*b.Y - b.X*a.Y
	}
	return twice / 2
}

// BoundsOf returns the axis-aligned bounding box of points.
func BoundsOf(pts []model.FPoint) model.BBox {
	b := model.BBox{MinX: pts[0].X, MinY: pts[0].Y, MaxX: pts[0].X, MaxY: pts[0].Y}
	for _, p := range pts[1:] {
		if p.X < b.MinX {
			b.MinX = p.X
		}
		if p.X > b.MaxX {
			b.MaxX = p.X
		}
		if p.Y < b.MinY {
			b.MinY = p.Y
		}
		if p.Y > b.MaxY {
			b.MaxY = p.Y
		}
	}
	return b
}

// SegmentIntersects reports whether open segments ab and cd cross in their
// interiors. Shared endpoints are not counted as self-intersection.
func SegmentIntersects(a, b, c, d model.FPoint) bool {
	den := (b.X-a.X)*(d.Y-c.Y) - (b.Y-a.Y)*(d.X-c.X)
	if math.Abs(den) < epsilon {
		return false
	}
	t := ((c.X-a.X)*(d.Y-c.Y) - (c.Y-a.Y)*(d.X-c.X)) / den
	u := ((c.X-a.X)*(b.Y-a.Y) - (c.Y-a.Y)*(b.X-a.X)) / den
	return t > epsilon && t < 1-epsilon && u > epsilon && u < 1-epsilon
}

// zeroLengthOpenEdge checks only internal edges of an open polyline.
func zeroLengthOpenEdge(pts []model.FPoint) int {
	for i := 0; i+1 < len(pts); i++ {
		if dist(pts[i], pts[i+1]) < epsilon {
			return i
		}
	}
	return -1
}

// selfIntersectOpen checks non-adjacent edges of an open polyline.
func selfIntersectOpen(pts []model.FPoint) (bool, int, int) {
	n := len(pts)
	for i := 0; i+1 < n; i++ {
		for j := i + 2; j+1 < n; j++ {
			if SegmentIntersects(pts[i], pts[i+1], pts[j], pts[j+1]) {
				return true, i, j
			}
		}
	}
	return false, -1, -1
}
