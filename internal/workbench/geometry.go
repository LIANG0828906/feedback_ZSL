package workbench

import (
	"math"
	"sort"
)

const geometryEpsilon = 1e-8

func nearlyEqual(a, b float64) bool {
	return math.Abs(a-b) <= geometryEpsilon
}

func pointsEqual(a, b Vec2) bool {
	return nearlyEqual(a.X, b.X) && nearlyEqual(a.Y, b.Y)
}

func cross(a, b, c Vec2) float64 {
	return (b.X-a.X)*(c.Y-a.Y) - (b.Y-a.Y)*(c.X-a.X)
}

func signedArea(points []Vec2) float64 {
	if len(points) < 3 {
		return 0
	}
	sum := 0.0
	for i := 0; i < len(points); i++ {
		j := (i + 1) % len(points)
		sum += points[i].X*points[j].Y - points[j].X*points[i].Y
	}
	return sum / 2
}

func polygonArea(points []Vec2) float64 {
	return math.Abs(signedArea(points))
}

func openPolygon(points []Vec2) []Vec2 {
	if len(points) > 1 && pointsEqual(points[0], points[len(points)-1]) {
		return points[:len(points)-1]
	}
	return points
}

func pointOnSegment(p, a, b Vec2) bool {
	if math.Abs(cross(a, b, p)) > geometryEpsilon {
		return false
	}
	return p.X >= math.Min(a.X, b.X)-geometryEpsilon &&
		p.X <= math.Max(a.X, b.X)+geometryEpsilon &&
		p.Y >= math.Min(a.Y, b.Y)-geometryEpsilon &&
		p.Y <= math.Max(a.Y, b.Y)+geometryEpsilon
}

func pointInPolygon(p Vec2, polygon []Vec2) bool {
	points := openPolygon(polygon)
	if len(points) < 3 {
		return false
	}
	inside := false
	for i, j := 0, len(points)-1; i < len(points); j, i = i, i+1 {
		a, b := points[j], points[i]
		if pointOnSegment(p, a, b) {
			return true
		}
		if (a.Y > p.Y) != (b.Y > p.Y) {
			x := (b.X-a.X)*(p.Y-a.Y)/(b.Y-a.Y) + a.X
			if p.X < x {
				inside = !inside
			}
		}
	}
	return inside
}

func pointInBounds(p Vec2, b Bounds) bool {
	return p.X >= b.MinX-geometryEpsilon && p.X <= b.MaxX+geometryEpsilon &&
		p.Y >= b.MinY-geometryEpsilon && p.Y <= b.MaxY+geometryEpsilon
}

func segmentIntersection(a, b, c, d Vec2) bool {
	o1 := cross(a, b, c)
	o2 := cross(a, b, d)
	o3 := cross(c, d, a)
	o4 := cross(c, d, b)
	if math.Abs(o1) <= geometryEpsilon && pointOnSegment(c, a, b) {
		return true
	}
	if math.Abs(o2) <= geometryEpsilon && pointOnSegment(d, a, b) {
		return true
	}
	if math.Abs(o3) <= geometryEpsilon && pointOnSegment(a, c, d) {
		return true
	}
	if math.Abs(o4) <= geometryEpsilon && pointOnSegment(b, c, d) {
		return true
	}
	return (o1 > 0) != (o2 > 0) && (o3 > 0) != (o4 > 0)
}

func polygonSelfIntersects(polygon []Vec2) bool {
	points := openPolygon(polygon)
	n := len(points)
	if n < 3 {
		return false
	}
	for i := 0; i < n; i++ {
		a := points[i]
		b := points[(i+1)%n]
		if pointsEqual(a, b) {
			return true
		}
	}
	for i := 0; i < n; i++ {
		a, b := points[i], points[(i+1)%n]
		for j := i + 1; j < n; j++ {
			if j == i || j == (i+1)%n || i == (j+1)%n {
				continue
			}
			c, d := points[j], points[(j+1)%n]
			if segmentIntersection(a, b, c, d) {
				return true
			}
		}
	}
	return false
}

func validatePolygon(points []Vec2, objectID, field string) *Error {
	if len(points) == 0 {
		return schemaError(field, "polygon is required")
	}
	if len(points) < 4 {
		return geomError(objectID, "polygon must contain at least three vertices and a closing vertex")
	}
	if !pointsEqual(points[0], points[len(points)-1]) {
		return geomError(objectID, "polygon is not closed; first and last vertices differ")
	}
	if polygonSelfIntersects(points) {
		return geomError(objectID, "polygon has duplicate vertices, zero-length edges, or self-intersection")
	}
	if polygonArea(points) <= geometryEpsilon {
		return geomError(objectID, "polygon area is degenerate")
	}
	return nil
}

func ensureCCW(points []Vec2) []Vec2 {
	out := append([]Vec2(nil), points...)
	if signedArea(out) < 0 {
		for i, j := 0, len(out)-1; i < j; i, j = i+1, j-1 {
			out[i], out[j] = out[j], out[i]
		}
	}
	return out
}

func pointInTriangle(p, a, b, c Vec2) bool {
	d1 := cross(a, b, p)
	d2 := cross(b, c, p)
	d3 := cross(c, a, p)
	hasNeg := d1 < -geometryEpsilon || d2 < -geometryEpsilon || d3 < -geometryEpsilon
	hasPos := d1 > geometryEpsilon || d2 > geometryEpsilon || d3 > geometryEpsilon
	return !(hasNeg && hasPos)
}

// triangulate implements deterministic ear clipping for a simple polygon.
// It is used only for exact polygon overlap checks and intersection areas.
func triangulate(polygon []Vec2) ([][3]Vec2, bool) {
	points := ensureCCW(openPolygon(polygon))
	if len(points) < 3 {
		return nil, false
	}
	indices := make([]int, len(points))
	for i := range indices {
		indices[i] = i
	}
	triangles := make([][3]Vec2, 0, len(points)-2)
	guard := 0
	for len(indices) > 3 && guard < len(points)*len(points)+16 {
		guard++
		earFound := false
		for pos := 0; pos < len(indices); pos++ {
			prev := indices[(pos+len(indices)-1)%len(indices)]
			curr := indices[pos]
			next := indices[(pos+1)%len(indices)]
			a, b, c := points[prev], points[curr], points[next]
			if cross(a, b, c) <= geometryEpsilon {
				continue
			}
			contains := false
			for _, idx := range indices {
				if idx == prev || idx == curr || idx == next {
					continue
				}
				if pointInTriangle(points[idx], a, b, c) {
					contains = true
					break
				}
			}
			if contains {
				continue
			}
			triangles = append(triangles, [3]Vec2{a, b, c})
			indices = append(indices[:pos], indices[pos+1:]...)
			earFound = true
			break
		}
		if !earFound {
			return nil, false
		}
	}
	if len(indices) == 3 {
		triangles = append(triangles, [3]Vec2{
			points[indices[0]], points[indices[1]], points[indices[2]],
		})
	}
	return triangles, len(triangles) > 0
}

func clipPolygonToTriangle(subject []Vec2, clip [3]Vec2) []Vec2 {
	output := append([]Vec2(nil), subject...)
	clipPoints := ensureCCW(clip[:])
	for i := 0; i < len(clipPoints); i++ {
		a := clipPoints[i]
		b := clipPoints[(i+1)%len(clipPoints)]
		if len(output) == 0 {
			break
		}
		input := output
		output = nil
		s := input[len(input)-1]
		for _, e := range input {
			eInside := cross(a, b, e) >= -geometryEpsilon
			sInside := cross(a, b, s) >= -geometryEpsilon
			if eInside {
				if !sInside {
					output = append(output, lineIntersection(s, e, a, b))
				}
				output = append(output, e)
			} else if sInside {
				output = append(output, lineIntersection(s, e, a, b))
			}
			s = e
		}
	}
	return output
}

func lineIntersection(p1, p2, p3, p4 Vec2) Vec2 {
	denom := (p1.X-p2.X)*(p3.Y-p4.Y) - (p1.Y-p2.Y)*(p3.X-p4.X)
	if math.Abs(denom) <= geometryEpsilon {
		return p2
	}
	det1 := p1.X*p2.Y - p1.Y*p2.X
	det2 := p3.X*p4.Y - p3.Y*p4.X
	x := (det1*(p3.X-p4.X) - (p1.X-p2.X)*det2) / denom
	y := (det1*(p3.Y-p4.Y) - (p1.Y-p2.Y)*det2) / denom
	return Vec2{X: x, Y: y}
}

func triangleIntersectionArea(a, b [3]Vec2) float64 {
	poly := clipPolygonToTriangle(a[:], b)
	if len(poly) < 3 {
		return 0
	}
	return polygonArea(poly)
}

func polygonIntersectionArea(a, b []Vec2) float64 {
	ta, okA := triangulate(a)
	tb, okB := triangulate(b)
	if !okA || !okB {
		return polygonGridIntersectionArea(a, b, 256)
	}
	area := 0.0
	for _, x := range ta {
		for _, y := range tb {
			area += triangleIntersectionArea(x, y)
		}
	}
	return area
}

func polygonGridIntersectionArea(a, b []Vec2, resolution int) float64 {
	all := append(append([]Vec2(nil), a...), b...)
	minX, minY := math.Inf(1), math.Inf(1)
	maxX, maxY := math.Inf(-1), math.Inf(-1)
	for _, p := range all {
		minX = math.Min(minX, p.X)
		minY = math.Min(minY, p.Y)
		maxX = math.Max(maxX, p.X)
		maxY = math.Max(maxY, p.Y)
	}
	dx := (maxX - minX) / float64(resolution)
	dy := (maxY - minY) / float64(resolution)
	count := 0
	for row := 0; row < resolution; row++ {
		for col := 0; col < resolution; col++ {
			p := Vec2{X: minX + (float64(col)+0.5)*dx, Y: minY + (float64(row)+0.5)*dy}
			if pointInPolygon(p, a) && pointInPolygon(p, b) {
				count++
			}
		}
	}
	return dx * dy * float64(count)
}

func polygonOverlaps(a, b []Vec2) bool {
	if polygonIntersectionArea(a, b) > 1e-7 {
		return true
	}
	return false
}

func sortedUniqueStrings(values []string) []string {
	seen := make(map[string]struct{}, len(values))
	out := make([]string, 0, len(values))
	for _, v := range values {
		if _, ok := seen[v]; ok {
			continue
		}
		seen[v] = struct{}{}
		out = append(out, v)
	}
	sort.Strings(out)
	return out
}
