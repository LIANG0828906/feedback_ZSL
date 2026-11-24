package wb

import "sort"

func manhattan(a, b Point) int {
	return absInt(a.X-b.X) + absInt(a.Y-b.Y)
}

func minMaxInt(a, b int) (int, int) {
	if a < b {
		return a, b
	}
	return b, a
}

// onSegment assumes collinearity and checks bounding-box containment.
func onSegment(p, a, b Point) bool {
	return p.X >= minInt(a.X, b.X) && p.X <= maxInt(a.X, b.X) &&
		p.Y >= minInt(a.Y, b.Y) && p.Y <= maxInt(a.Y, b.Y)
}

func minInt(a, b int) int {
	if a < b {
		return a
	}
	return b
}

func maxInt(a, b int) int {
	if a > b {
		return a
	}
	return b
}

func cross(a, b, c Point) int {
	return (b.X-a.X)*(c.Y-a.Y) - (b.Y-a.Y)*(c.X-a.X)
}

// segmentsProperlyCross reports a crossing through the interior of both
// segments. Endpoint and collinear contacts are not "crossing through".
func segmentsProperlyCross(a, b, c, d Point) bool {
	c1 := cross(a, b, c)
	c2 := cross(a, b, d)
	c3 := cross(c, d, a)
	c4 := cross(c, d, b)
	return ((c1 > 0 && c2 < 0) || (c1 < 0 && c2 > 0)) &&
		((c3 > 0 && c4 < 0) || (c3 < 0 && c4 > 0))
}

func pointInInterval(p Point, o Opening) bool {
	if o.Horizontal() {
		lo, hi := minMaxInt(o.X1, o.X2)
		return p.Y == o.Y1 && p.X >= lo && p.X <= hi
	}
	lo, hi := minMaxInt(o.Y1, o.Y2)
	return p.X == o.X1 && p.Y >= lo && p.Y <= hi
}

// segmentHitsWallOutsideOpening returns true only when the tested segment
// properly crosses a wall away from an opening interval.
func segmentHitsWallOutsideOpening(a, b Point, w Wall, openings []Opening) (Point, bool) {
	c := Point{X: w.X1, Y: w.Y1}
	d := Point{X: w.X2, Y: w.Y2}
	if !segmentsProperlyCross(a, b, c, d) {
		return Point{}, false
	}
	hit := intersectionPointApprox(a, b, c, d)
	for _, o := range openings {
		if o.WallID != w.ID {
			continue
		}
		if pointInInterval(hit, o) {
			return Point{}, false
		}
	}
	return hit, true
}

// intersectionPointApprox returns the integer crossing point for the
// axis-aligned walls used by this workbench. Diagonal test segments cross a
// horizontal/vertical wall at one fixed coordinate; rounding is only used for
// UI markers, while all length arithmetic remains Manhattan/integer.
func intersectionPointApprox(a, b, c, d Point) Point {
	if c.Y == d.Y {
		y := c.Y
		dy := b.Y - a.Y
		if dy == 0 {
			return Point{X: a.X, Y: y}
		}
		// x = a.X + (y-a.Y)*(b.X-a.X)/dy, rounded to nearest integer.
		num := (y - a.Y) * (b.X - a.X)
		x := a.X + roundedDiv(num, dy)
		return Point{X: x, Y: y}
	}
	x := c.X
	dx := b.X - a.X
	if dx == 0 {
		return Point{X: x, Y: a.Y}
	}
	num := (x - a.X) * (b.Y - a.Y)
	y := a.Y + roundedDiv(num, dx)
	return Point{X: x, Y: y}
}

func roundedDiv(num, den int) int {
	if den < 0 {
		num = -num
		den = -den
	}
	if num >= 0 {
		return (num + den/2) / den
	}
	return -((-num + den/2) / den)
}

func polylineCrossesWall(points []Point, walls []Wall, openings []Opening) (Point, string, bool) {
	for i := 0; i+1 < len(points); i++ {
		for _, w := range walls {
			if hit, ok := segmentHitsWallOutsideOpening(points[i], points[i+1], w, openings); ok {
				return hit, w.ID, true
			}
		}
	}
	return Point{}, "", false
}

func sameStringSet(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	ca := append([]string(nil), a...)
	cb := append([]string(nil), b...)
	sort.Strings(ca)
	sort.Strings(cb)
	for i := range ca {
		if ca[i] != cb[i] {
			return false
		}
	}
	return true
}

func sortedKeys[V any](m map[string]V) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}
