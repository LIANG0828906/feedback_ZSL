package geom

import "roomreverb/internal/model"

func PointInRect(p model.Point, r model.Rect) bool {
	return p.X >= r.Min.X && p.X <= r.Max.X && p.Y >= r.Min.Y && p.Y <= r.Max.Y
}

func PointInRectInterior(p model.Point, r model.Rect) bool {
	return p.X > r.Min.X && p.X < r.Max.X && p.Y > r.Min.Y && p.Y < r.Max.Y
}

func LocateRoom(level *model.Level, p model.Point) string {
	for _, room := range level.Rooms {
		if PointInRectInterior(p, room.Rect) {
			return room.ID
		}
	}
	for _, room := range level.Rooms {
		if PointInRect(p, room.Rect) {
			return room.ID
		}
	}
	return ""
}

func RectsOverlapInterior(a, b model.Rect) bool {
	return a.Min.X < b.Max.X && a.Max.X > b.Min.X &&
		a.Min.Y < b.Max.Y && a.Max.Y > b.Min.Y
}

func OnSegment(p, a, b model.Point) bool {
	return Cross(a, b, p) == 0 &&
		p.X >= min(a.X, b.X) && p.X <= max(a.X, b.X) &&
		p.Y >= min(a.Y, b.Y) && p.Y <= max(a.Y, b.Y)
}

func Cross(a, b, c model.Point) int64 {
	return int64(b.X-a.X)*int64(c.Y-a.Y) - int64(b.Y-a.Y)*int64(c.X-a.X)
}

func SegmentsIntersect(a1, a2, b1, b2 model.Point) bool {
	if OnSegment(a1, b1, b2) || OnSegment(a2, b1, b2) ||
		OnSegment(b1, a1, a2) || OnSegment(b2, a1, a2) {
		return true
	}
	c1 := Cross(a1, a2, b1)
	c2 := Cross(a1, a2, b2)
	c3 := Cross(b1, b2, a1)
	c4 := Cross(b1, b2, a2)
	return ((c1 > 0 && c2 < 0) || (c1 < 0 && c2 > 0)) &&
		((c3 > 0 && c4 < 0) || (c3 < 0 && c4 > 0))
}

func SegmentOnAxisAlignedBoundary(a, b model.Point, r model.Rect) bool {
	if a.X == b.X {
		x := a.X
		if x != r.Min.X && x != r.Max.X {
			return false
		}
		return min(a.Y, b.Y) >= r.Min.Y && max(a.Y, b.Y) <= r.Max.Y
	}
	if a.Y == b.Y {
		y := a.Y
		if y != r.Min.Y && y != r.Max.Y {
			return false
		}
		return min(a.X, b.X) >= r.Min.X && max(a.X, b.X) <= r.Max.X
	}
	return false
}

func SegmentContains(outerA, outerB, innerA, innerB model.Point) bool {
	if outerA.X == outerB.X {
		if innerA.X != outerA.X || innerB.X != outerA.X {
			return false
		}
		return min(innerA.Y, innerB.Y) >= min(outerA.Y, outerB.Y) &&
			max(innerA.Y, innerB.Y) <= max(outerA.Y, outerB.Y)
	}
	if outerA.Y == outerB.Y {
		if innerA.Y != outerA.Y || innerB.Y != outerA.Y {
			return false
		}
		return min(innerA.X, innerB.X) >= min(outerA.X, outerB.X) &&
			max(innerA.X, innerB.X) <= max(outerA.X, outerB.X)
	}
	return false
}

func DoorMidpoint(d model.Door) model.Point {
	return model.Point{X: (d.A.X + d.B.X) / 2, Y: (d.A.Y + d.B.Y) / 2}
}
