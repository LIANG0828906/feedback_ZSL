package acoustics

import (
	"fmt"
	"math"

	"roomacoustics/pkg/fixedpoint"
)

const geomEpsilon = 1e-7

// Validate performs schema-independent semantic and geometry validation.
func Validate(l *Level) error {
	ve := &ValidationError{}
	if l == nil {
		return &DomainError{Code: CodeSchema, ObjectID: "document", Message: "level is nil"}
	}

	roomByID := map[string]Room{}
	seenIDs := map[string]string{}
	add := func(code, id, field, format string, args ...any) {
		ve.Issues = append(ve.Issues, Issue{Code: code, ObjectID: id, Field: field, Message: fmt.Sprintf(format, args...)})
	}
	uniqueID := func(kind, id string) bool {
		if id == "" {
			return false
		}
		if prev, ok := seenIDs[id]; ok {
			add(CodeSchema, id, "id", "%s id duplicates %s id", kind, prev)
			return false
		}
		seenIDs[id] = kind
		return true
	}

	if len(l.Rooms) == 0 {
		add(CodeSchema, "level", "rooms", "at least one room is required")
	}
	for _, r := range l.Rooms {
		uniqueID("room", r.ID)
		if _, exists := roomByID[r.ID]; exists {
			continue
		}
		roomByID[r.ID] = r
		validateRectBounds(r.ID, r.Rect, add)
		if r.Rect.X2-r.Rect.X1 <= geomEpsilon || r.Rect.Y2-r.Rect.Y1 <= geomEpsilon {
			add(CodeGeom, r.ID, "rect", "room rectangle must have positive width and height")
		}
	}

	for i := 0; i < len(l.Rooms); i++ {
		for j := i + 1; j < len(l.Rooms); j++ {
			a, b := l.Rooms[i], l.Rooms[j]
			overlapX := math.Min(a.Rect.X2, b.Rect.X2) - math.Max(a.Rect.X1, b.Rect.X1)
			overlapY := math.Min(a.Rect.Y2, b.Rect.Y2) - math.Max(a.Rect.Y1, b.Rect.Y1)
			if overlapX > geomEpsilon && overlapY > geomEpsilon {
				add(CodeGeom, a.ID+","+b.ID, "rect", "rooms overlap by %.6fm x %.6fm", overlapX, overlapY)
			}
		}
	}

	wallByID := map[string]Wall{}
	for _, w := range l.Walls {
		uniqueID("wall", w.ID)
		if _, ok := roomByID[w.RoomA]; !ok {
			add(CodeSchema, w.ID, "room_a", "unknown room %q", w.RoomA)
		}
		if _, ok := roomByID[w.RoomB]; !ok {
			add(CodeSchema, w.ID, "room_b", "unknown room %q", w.RoomB)
		}
		validatePointBounds(w.ID, "a", w.A, add)
		validatePointBounds(w.ID, "b", w.B, add)
		if distancePoint(w.A, w.B) <= geomEpsilon {
			add(CodeGeom, w.ID, "segment", "wall segment must have positive length")
			continue
		}
		if !almostEqual(w.A.X, w.B.X) && !almostEqual(w.A.Y, w.B.Y) {
			add(CodeGeom, w.ID, "segment", "shared wall must be axis-aligned")
		}
		if ra, okA := roomByID[w.RoomA]; okA {
			if !segmentOnRoomBoundary(w, ra.Rect) {
				add(CodeGeom, w.ID, "room_a", "wall segment is not on boundary of %s", w.RoomA)
			}
		}
		if rb, okB := roomByID[w.RoomB]; okB {
			if !segmentOnRoomBoundary(w, rb.Rect) {
				add(CodeGeom, w.ID, "room_b", "wall segment is not on boundary of %s", w.RoomB)
			}
		}
		wallByID[w.ID] = w
	}

	for i := 0; i < len(l.Walls); i++ {
		for j := i + 1; j < len(l.Walls); j++ {
			a, b := l.Walls[i], l.Walls[j]
			if overlap, axis := wallOverlapLength(a, b); overlap > geomEpsilon {
				add(CodeGeom, a.ID+","+b.ID, "segment", "wall segments overlap on %s axis by %.6fm", axis, overlap)
			}
		}
	}

	for _, d := range l.Doors {
		uniqueID("door", d.ID)
		w, ok := wallByID[d.WallID]
		if !ok {
			add(CodeSchema, d.ID, "wall_id", "unknown wall %q", d.WallID)
			continue
		}
		if d.Width <= geomEpsilon {
			add(CodeGeom, d.ID, "width_m", "door width must be positive")
		}
		validatePointBounds(d.ID, "center", d.Center, add)
		if !doorInsideWall(d, w) {
			add(CodeGeom, d.ID, "center/width_m", "door opening is outside wall %s", d.WallID)
		}
	}

	for _, r := range l.Reflectors {
		uniqueID("reflector", r.ID)
		room, ok := roomByID[r.RoomID]
		if !ok {
			add(CodeSchema, r.ID, "room_id", "unknown room %q", r.RoomID)
		} else {
			if !pointInRect(r.A, room.Rect, 1e-6) {
				add(CodeGeom, r.ID, "a", "reflector endpoint is outside room %s", r.RoomID)
			}
			if !pointInRect(r.B, room.Rect, 1e-6) {
				add(CodeGeom, r.ID, "b", "reflector endpoint is outside room %s", r.RoomID)
			}
		}
		validatePointBounds(r.ID, "a", r.A, add)
		validatePointBounds(r.ID, "b", r.B, add)
		validatePointBounds(r.ID, "normal", r.Normal, add)
		segLen := distancePoint(r.A, r.B)
		if segLen <= geomEpsilon {
			add(CodeGeom, r.ID, "segment", "reflector segment must have positive length")
		}
		normalLen := math.Hypot(r.Normal.X, r.Normal.Y)
		if math.Abs(normalLen-1) > 1e-5 {
			add(CodeGeom, r.ID, "normal", "normal must be unit length, got %.8f", normalLen)
		}
		if segLen > geomEpsilon && normalLen > geomEpsilon {
			dx := (r.B.X - r.A.X) / segLen
			dy := (r.B.Y - r.A.Y) / segLen
			perp := math.Abs(dx*r.Normal.X + dy*r.Normal.Y)
			if perp > 1e-5 {
				add(CodeGeom, r.ID, "normal", "normal must be perpendicular to reflector, dot=%.8f", perp)
			}
		}
		if math.IsNaN(r.Absorption) || math.IsInf(r.Absorption, 0) || r.Absorption < 0 || r.Absorption >= 1 {
			add(CodeGeom, r.ID, "absorption", "absorption must be in [0,1), got %v", r.Absorption)
		}
	}

	for _, s := range l.Sources {
		uniqueID("source", s.ID)
		room, ok := roomByID[s.RoomID]
		if !ok {
			add(CodeSchema, s.ID, "room_id", "unknown room %q", s.RoomID)
		} else if !pointInRect(s.Position, room.Rect, 1e-6) {
			add(CodeGeom, s.ID, "position", "source is outside room %s", s.RoomID)
		}
		validatePointBounds(s.ID, "position", s.Position, add)
		if math.IsNaN(s.PowerDB) || math.IsInf(s.PowerDB, 0) {
			add(CodeGeom, s.ID, "power_db", "power must be finite")
		}
	}

	for _, lis := range l.Listeners {
		uniqueID("listener", lis.ID)
		room, ok := roomByID[lis.RoomID]
		if !ok {
			add(CodeSchema, lis.ID, "room_id", "unknown room %q", lis.RoomID)
		} else if !pointInRect(lis.Position, room.Rect, 1e-6) {
			add(CodeGeom, lis.ID, "position", "listener is outside room %s", lis.RoomID)
		}
		validatePointBounds(lis.ID, "position", lis.Position, add)
	}

	return ve.OrNil()
}

func validatePointBounds(id, field string, p Point, add func(string, string, string, string, ...any)) {
	if math.IsNaN(p.X) || math.IsNaN(p.Y) || math.IsInf(p.X, 0) || math.IsInf(p.Y, 0) {
		add(CodeGeom, id, field, "coordinate must be finite")
		return
	}
	if math.Abs(p.X) > fixedpoint.MaxCoordinateM || math.Abs(p.Y) > fixedpoint.MaxCoordinateM {
		add(CodeGeom, id, field, "coordinate (%.3f,%.3f) exceeds +/-%.0fm", p.X, p.Y, fixedpoint.MaxCoordinateM)
	}
}

func validateRectBounds(id string, r Rect, add func(string, string, string, string, ...any)) {
	for _, p := range []Point{r.Min(), r.Max()} {
		validatePointBounds(id, "rect", p, add)
	}
}

func almostEqual(a, b float64) bool { return math.Abs(a-b) <= geomEpsilon }

func distancePoint(a, b Point) float64 { return math.Hypot(a.X-b.X, a.Y-b.Y) }

func pointInRect(p Point, r Rect, tol float64) bool {
	return p.X >= r.X1-tol && p.X <= r.X2+tol && p.Y >= r.Y1-tol && p.Y <= r.Y2+tol
}

func pointOnLine(p, a, b Point) bool {
	if almostEqual(a.X, b.X) {
		return almostEqual(p.X, a.X) && between(p.Y, math.Min(a.Y, b.Y), math.Max(a.Y, b.Y))
	}
	if almostEqual(a.Y, b.Y) {
		return almostEqual(p.Y, a.Y) && between(p.X, math.Min(a.X, b.X), math.Max(a.X, b.X))
	}
	return false
}

func between(v, lo, hi float64) bool {
	return v >= lo-geomEpsilon && v <= hi+geomEpsilon
}

func segmentOnRoomBoundary(w Wall, r Rect) bool {
	if almostEqual(w.A.X, w.B.X) {
		x := w.A.X
		if !almostEqual(x, r.X1) && !almostEqual(x, r.X2) {
			return false
		}
		lo, hi := math.Min(w.A.Y, w.B.Y), math.Max(w.A.Y, w.B.Y)
		return lo >= r.Y1-geomEpsilon && hi <= r.Y2+geomEpsilon
	}
	if almostEqual(w.A.Y, w.B.Y) {
		y := w.A.Y
		if !almostEqual(y, r.Y1) && !almostEqual(y, r.Y2) {
			return false
		}
		lo, hi := math.Min(w.A.X, w.B.X), math.Max(w.A.X, w.B.X)
		return lo >= r.X1-geomEpsilon && hi <= r.X2+geomEpsilon
	}
	return false
}

func wallOverlapLength(a, b Wall) (float64, string) {
	if almostEqual(a.A.X, a.B.X) && almostEqual(b.A.X, b.B.X) && almostEqual(a.A.X, b.A.X) {
		lo := math.Max(math.Min(a.A.Y, a.B.Y), math.Min(b.A.Y, b.B.Y))
		hi := math.Min(math.Max(a.A.Y, a.B.Y), math.Max(b.A.Y, b.B.Y))
		return hi - lo, "y"
	}
	if almostEqual(a.A.Y, a.B.Y) && almostEqual(b.A.Y, b.B.Y) && almostEqual(a.A.Y, b.A.Y) {
		lo := math.Max(math.Min(a.A.X, a.B.X), math.Min(b.A.X, b.B.X))
		hi := math.Min(math.Max(a.A.X, a.B.X), math.Max(b.A.X, b.B.X))
		return hi - lo, "x"
	}
	return 0, ""
}

func doorInsideWall(d Door, w Wall) bool {
	if !pointOnLine(d.Center, w.A, w.B) {
		return false
	}
	half := d.Width / 2
	if almostEqual(w.A.X, w.B.X) {
		lo := math.Min(w.A.Y, w.B.Y)
		hi := math.Max(w.A.Y, w.B.Y)
		return d.Center.Y-half >= lo-geomEpsilon && d.Center.Y+half <= hi+geomEpsilon
	}
	if almostEqual(w.A.Y, w.B.Y) {
		lo := math.Min(w.A.X, w.B.X)
		hi := math.Max(w.A.X, w.B.X)
		return d.Center.X-half >= lo-geomEpsilon && d.Center.X+half <= hi+geomEpsilon
	}
	return false
}
