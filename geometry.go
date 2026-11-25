package acoustic

import (
	"math"
)

type segment struct {
	A Point
	B Point
}

func distanceTicks(a, b Point) (int64, float64) {
	dx := float64(b.X - a.X)
	dy := float64(b.Y - a.Y)
	exact := math.Hypot(dx, dy)
	rounded := int64(math.Round(exact))
	return rounded, exact - float64(rounded)
}

func dotPoint(a, b Point) int64 {
	return a.X*b.X + a.Y*b.Y
}

func crossPoint(a, b Point) int64 {
	return a.X*b.Y - a.Y*b.X
}

func pointLess(a, b Point) bool {
	if a.X != b.X {
		return a.X < b.X
	}
	return a.Y < b.Y
}

func pointSub(a, b Point) Point {
	return Point{X: a.X - b.X, Y: a.Y - b.Y}
}

func pointAdd(a, b Point) Point {
	return Point{X: a.X + b.X, Y: a.Y + b.Y}
}

func pointMul(a Point, n int64) Point {
	return Point{X: a.X * n, Y: a.Y * n}
}

func rectsOverlapInterior(a, b Rect) bool {
	width := minInt64(a.MaxX, b.MaxX) - maxInt64(a.MinX, b.MinX)
	height := minInt64(a.MaxY, b.MaxY) - maxInt64(a.MinY, b.MinY)
	return width > 0 && height > 0
}

func rectsTouchOnlyAtPoint(a, b Rect) bool {
	points := [][2]Fixed{
		{a.MaxX, a.MaxY},
		{a.MaxX, a.MinY},
		{a.MinX, a.MaxY},
		{a.MinX, a.MinY},
	}
	corners := [][2]Fixed{
		{b.MinX, b.MinY},
		{b.MinX, b.MaxY},
		{b.MaxX, b.MinY},
		{b.MaxX, b.MaxY},
	}
	matches := 0
	for _, p := range points {
		for _, q := range corners {
			if p[0] == q[0] && p[1] == q[1] {
				matches++
			}
		}
	}
	return matches == 1
}

type sharedBoundary struct {
	Vertical bool
	Value    Fixed
	Min      Fixed
	Max      Fixed
}

func sharedRectBoundary(a, b Rect) (sharedBoundary, bool) {
	var candidates []sharedBoundary
	if a.MaxX == b.MinX || b.MaxX == a.MinX {
		value := a.MaxX
		if b.MaxX == a.MinX {
			value = b.MaxX
		}
		lo := maxInt64(a.MinY, b.MinY)
		hi := minInt64(a.MaxY, b.MaxY)
		if hi > lo {
			candidates = append(candidates, sharedBoundary{Vertical: true, Value: value, Min: lo, Max: hi})
		}
	}
	if a.MaxY == b.MinY || b.MaxY == a.MinY {
		value := a.MaxY
		if b.MaxY == a.MinY {
			value = b.MaxY
		}
		lo := maxInt64(a.MinX, b.MinX)
		hi := minInt64(a.MaxX, b.MaxX)
		if hi > lo {
			candidates = append(candidates, sharedBoundary{Vertical: false, Value: value, Min: lo, Max: hi})
		}
	}
	if len(candidates) != 1 {
		return sharedBoundary{}, false
	}
	return candidates[0], true
}

func wallSharedBoundary(roomA, roomB Room, wall Wall) (sharedBoundary, bool) {
	boundary, ok := sharedRectBoundary(roomA.Rect, roomB.Rect)
	if !ok {
		return sharedBoundary{}, false
	}
	a, b := wall.A, wall.B
	if pointLess(b, a) {
		a, b = b, a
	}
	if boundary.Vertical {
		if a.X != boundary.Value || b.X != boundary.Value || a.Y >= b.Y {
			return sharedBoundary{}, false
		}
		if a.Y < boundary.Min || b.Y > boundary.Max {
			return sharedBoundary{}, false
		}
	} else {
		if a.Y != boundary.Value || b.Y != boundary.Value || a.X >= b.X {
			return sharedBoundary{}, false
		}
		if a.X < boundary.Min || b.X > boundary.Max {
			return sharedBoundary{}, false
		}
	}
	if a == b {
		return sharedBoundary{}, false
	}
	return boundary, true
}

func wallLength(w Wall) Fixed {
	if w.A.X == w.B.X {
		if w.B.Y >= w.A.Y {
			return w.B.Y - w.A.Y
		}
		return w.A.Y - w.B.Y
	}
	if w.B.X >= w.A.X {
		return w.B.X - w.A.X
	}
	return w.A.X - w.B.X
}

func openingSegment(w Wall, o Opening) (segment, bool) {
	if o.Width <= 0 {
		return segment{}, false
	}
	length := wallLength(w)
	if o.Start < 0 || o.Start+o.Width > length {
		return segment{}, false
	}
	if w.A.X == w.B.X {
		low := minInt64(w.A.Y, w.B.Y)
		return segment{A: Point{X: w.A.X, Y: low + o.Start}, B: Point{X: w.A.X, Y: low + o.Start + o.Width}}, true
	}
	low := minInt64(w.A.X, w.B.X)
	return segment{A: Point{X: low + o.Start, Y: w.A.Y}, B: Point{X: low + o.Start + o.Width, Y: w.A.Y}}, true
}

func openingCenter(w Wall, o Opening) (Point, bool) {
	seg, ok := openingSegment(w, o)
	if !ok {
		return Point{}, false
	}
	return Point{X: (seg.A.X + seg.B.X) / 2, Y: (seg.A.Y + seg.B.Y) / 2}, true
}

func pointOnSegmentTolerance(p, a, b Point, toleranceTicks int64) bool {
	ab := pointSub(b, a)
	ap := pointSub(p, a)
	lengthSquared := int64(ab.X*ab.X + ab.Y*ab.Y)
	if lengthSquared == 0 {
		return false
	}
	projection := int64(ap.X*ab.X + ap.Y*ab.Y)
	if projection < 0 || projection > lengthSquared {
		return false
	}
	cross := int64(ap.X*ab.Y - ap.Y*ab.X)
	length := math.Hypot(float64(ab.X), float64(ab.Y))
	perpendicular := math.Abs(float64(cross)) / length
	return perpendicular <= float64(toleranceTicks)
}

func minInt64(a, b int64) int64 {
	if a < b {
		return a
	}
	return b
}

func maxInt64(a, b int64) int64 {
	if a > b {
		return a
	}
	return b
}

func absInt64(v int64) int64 {
	if v < 0 {
		return -v
	}
	return v
}
