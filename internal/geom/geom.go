// Package geom provides deterministic planar geometry helpers used by the
// generator, loader and reconstruction stages.
package geom

import (
	"math"

	"collectionrecon/internal/errors"
	"collectionrecon/internal/model"
)

const eps = 1e-9

// BBox computes the axis aligned bounding box of a polygon.
func BBox(p model.Polygon) model.Rect {
	r := model.Rect{MinX: math.Inf(1), MinY: math.Inf(1), MaxX: math.Inf(-1), MaxY: math.Inf(-1)}
	for _, v := range p.Vertices {
		if v.X < r.MinX {
			r.MinX = v.X
		}
		if v.Y < r.MinY {
			r.MinY = v.Y
		}
		if v.X > r.MaxX {
			r.MaxX = v.X
		}
		if v.Y > r.MaxY {
			r.MaxY = v.Y
		}
	}
	return r
}

// IsClosed reports whether the final vertex repeats the first vertex.
func IsClosed(p model.Polygon) bool {
	n := len(p.Vertices)
	if n < 4 {
		return false
	}
	first, last := p.Vertices[0], p.Vertices[n-1]
	return math.Abs(first.X-last.X) < eps && math.Abs(first.Y-last.Y) < eps
}

func cross2(a, b, c model.Point) float64 {
	return (b.X-a.X)*(c.Y-a.Y) - (b.Y-a.Y)*(c.X-a.X)
}

// onSegment assumes collinearity and checks whether c lies on segment ab.
func onSegment(a, b, c model.Point) bool {
	return math.Min(a.X, b.X)-eps <= c.X && c.X <= math.Max(a.X, b.X)+eps &&
		math.Min(a.Y, b.Y)-eps <= c.Y && c.Y <= math.Max(a.Y, b.Y)+eps
}

func segmentsIntersect(a, b, c, d model.Point) bool {
	c1, c2 := cross2(a, b, c), cross2(a, b, d)
	c3, c4 := cross2(c, d, a), cross2(c, d, b)
	if c1*c2 < -eps && c3*c4 < -eps {
		return true
	}
	if math.Abs(c1) <= eps && onSegment(a, b, c) {
		return true
	}
	if math.Abs(c2) <= eps && onSegment(a, b, d) {
		return true
	}
	if math.Abs(c3) <= eps && onSegment(c, d, a) {
		return true
	}
	if math.Abs(c4) <= eps && onSegment(c, d, b) {
		return true
	}
	return false
}

// IsSimple reports whether the closed polygon has no self-intersecting edges.
// Adjacent edges only share their common endpoint.
func IsSimple(p model.Polygon) bool {
	n := len(p.Vertices)
	if n < 4 {
		return false
	}
	edges := make([][2]model.Point, 0, n-1)
	for i := 0; i < n-1; i++ {
		edges = append(edges, [2]model.Point{p.Vertices[i], p.Vertices[i+1]})
	}
	m := len(edges)
	for i := 0; i < m; i++ {
		for j := i + 1; j < m; j++ {
			adjacent := j == i+1 || (i == 0 && j == m-1)
			if adjacent {
				continue
			}
			if segmentsIntersect(edges[i][0], edges[i][1], edges[j][0], edges[j][1]) {
				return false
			}
		}
	}
	return true
}

// PointInPolygon uses ray casting; boundary points count as inside.
func PointInPolygon(p model.Point, poly model.Polygon) bool {
	n := len(poly.Vertices) - 1
	inside := false
	for i := 0; i < n; i++ {
		a, b := poly.Vertices[i], poly.Vertices[i+1]
		if math.Abs(cross2(a, b, p)) <= eps && onSegment(a, b, p) {
			return true
		}
		if (a.Y > p.Y) != (b.Y > p.Y) {
			xAt := a.X + (p.Y-a.Y)*(b.X-a.X)/(b.Y-a.Y)
			if xAt > p.X {
				inside = !inside
			}
		}
	}
	return inside
}

// PointInRect reports whether p lies inside r.
func PointInRect(p model.Point, r model.Rect) bool {
	return p.X >= r.MinX-eps && p.X <= r.MaxX+eps &&
		p.Y >= r.MinY-eps && p.Y <= r.MaxY+eps
}

// RectsOverlap reports strict interior overlap (touching edges are allowed).
func RectsOverlap(a, b model.Rect) bool {
	return a.MinX < b.MaxX-eps && b.MinX < a.MaxX-eps &&
		a.MinY < b.MaxY-eps && b.MinY < a.MaxY-eps
}

// RectInRect reports whether inner is contained in outer.
func RectInRect(inner, outer model.Rect) bool {
	return inner.MinX >= outer.MinX-eps && inner.MaxX <= outer.MaxX+eps &&
		inner.MinY >= outer.MinY-eps && inner.MaxY <= outer.MaxY+eps
}

// RectArea returns the area of a rectangle.
func RectArea(r model.Rect) float64 {
	return (r.MaxX - r.MinX) * (r.MaxY - r.MinY)
}

// RasterCompatible verifies that the raster dimensions match the region bbox
// given the declared square cell size.
func RasterCompatible(bbox model.Rect, cell float64, nx, ny int) error {
	if cell <= 0 || nx <= 0 || ny <= 0 {
		return errors.WithPath(errors.EGeom, "rasterCell",
			"raster dimensions must be positive: cell=%g nx=%d ny=%d", cell, nx, ny)
	}
	ex := math.Abs(cell*float64(nx) - (bbox.MaxX - bbox.MinX))
	ey := math.Abs(cell*float64(ny) - (bbox.MaxY - bbox.MinY))
	if ex > eps || ey > eps {
		return errors.WithContext(errors.EGeom, map[string]any{
			"dx": ex, "dy": ey,
		}, "raster size incompatible with region bbox")
	}
	return nil
}
