package model

import (
	"fmt"
	"sort"
)

type ValidateOptions struct {
	RequireTruth bool
}

func ValidateProject(p *Project, opts ValidateOptions) error {
	if p == nil {
		return SchemaError("project", "project is nil")
	}
	if p.SchemaVersion != SchemaVersion {
		return SchemaError("schema_version", "want %q, got %q", SchemaVersion, p.SchemaVersion)
	}
	if p.Bounds.W <= 0 || p.Bounds.H <= 0 {
		return GeometryError("", "", "bounds", "level bounds must have positive width and height")
	}
	if len(p.Rooms) == 0 {
		return SchemaError("rooms", "at least one room is required")
	}
	if len(p.Sources) == 0 {
		return SchemaError("sources", "at least one source is required")
	}
	if len(p.Listeners) == 0 {
		return SchemaError("listeners", "at least one listener is required")
	}
	if p.Tolerance.PathLengthMicro < 0 || p.Tolerance.CurveDBMicro < 0 || p.Tolerance.SPLDBMicro < 0 {
		return SchemaError("tolerance", "tolerances must be non-negative")
	}

	roomByID := make(map[string]Room, len(p.Rooms))
	for i, room := range p.Rooms {
		if room.ID == "" {
			return SchemaError(fmt.Sprintf("rooms[%d].id", i), "room id is required")
		}
		if room.Rect.W <= 0 || room.Rect.H <= 0 {
			e := GeometryError(room.ID, "", "rect", "room %s has non-positive width or height", room.ID)
			return e
		}
		if room.RT60Milliseconds <= 0 {
			e := SchemaError(fmt.Sprintf("rooms[%d].rt60_ms", i), "room %s must declare a positive RT60", room.ID)
			e.RoomID = room.ID
			return e
		}
		if room.EarlyDensityMilli < 0 {
			e := SchemaError(fmt.Sprintf("rooms[%d].early_reflection_density_milli", i), "room %s has negative early reflection density", room.ID)
			e.RoomID = room.ID
			return e
		}
		if room.ReverbGainMilli <= 0 {
			e := SchemaError(fmt.Sprintf("rooms[%d].reverb_gain_milli", i), "room %s must declare a positive reverb gain", room.ID)
			e.RoomID = room.ID
			return e
		}
		if _, exists := roomByID[room.ID]; exists {
			return GeometryError(room.ID, "", "id", "duplicate room id %s", room.ID)
		}
		if !rectInside(room.Rect, p.Bounds) {
			return GeometryError(room.ID, "", "rect", "room %s lies outside level bounds", room.ID)
		}
		roomByID[room.ID] = room
	}

	for i := 0; i < len(p.Rooms); i++ {
		for j := i + 1; j < len(p.Rooms); j++ {
			a, b := p.Rooms[i], p.Rooms[j]
			overlapX := min(a.Rect.Right(), b.Rect.Right()) - max(a.Rect.X, b.Rect.X)
			overlapY := min(a.Rect.Bottom(), b.Rect.Bottom()) - max(a.Rect.Y, b.Rect.Y)
			if overlapX > 0 && overlapY > 0 {
				return GeometryError(a.ID, "", "rect", "rooms %s and %s overlap with positive area", a.ID, b.ID)
			}
		}
	}

	wallByID := make(map[string]Wall, len(p.Walls))
	for i, wall := range p.Walls {
		if wall.ID == "" {
			return SchemaError(fmt.Sprintf("walls[%d].id", i), "wall id is required")
		}
		if wall.RoomA == "" || wall.RoomB == "" {
			return SchemaError(fmt.Sprintf("walls[%d]", i), "wall %s must reference two rooms", wall.ID)
		}
		if wall.RoomA == wall.RoomB {
			return GeometryError("", wall.ID, "rooms", "wall %s references the same room twice", wall.ID)
		}
		ra, okA := roomByID[wall.RoomA]
		rb, okB := roomByID[wall.RoomB]
		if !okA || !okB {
			return GeometryError("", wall.ID, "rooms", "wall %s references an unknown room", wall.ID)
		}
		if wall.X1 == wall.X2 && wall.Y1 == wall.Y2 {
			return GeometryError("", wall.ID, "segment", "wall %s has zero length", wall.ID)
		}
		if !wall.Horizontal() && !wall.Vertical() {
			return GeometryError("", wall.ID, "segment", "wall %s is not axis-aligned", wall.ID)
		}
		if !rectContainsPointInclusive(p.Bounds, wall.X1, wall.Y1) || !rectContainsPointInclusive(p.Bounds, wall.X2, wall.Y2) {
			return GeometryError("", wall.ID, "segment", "wall %s lies outside level bounds", wall.ID)
		}
		if !wallIsSharedEdge(ra.Rect, rb.Rect, wall) {
			return GeometryError("", wall.ID, "segment", "wall %s is not a shared edge of rooms %s and %s", wall.ID, wall.RoomA, wall.RoomB)
		}
		if _, exists := wallByID[wall.ID]; exists {
			return GeometryError("", wall.ID, "id", "duplicate wall id %s", wall.ID)
		}
		wallByID[wall.ID] = wall
	}

	doorByID := make(map[string]Door, len(p.Doors))
	doorsByWall := make(map[string][]Door)
	for i, door := range p.Doors {
		if door.ID == "" {
			return SchemaError(fmt.Sprintf("doors[%d].id", i), "door id is required")
		}
		if door.WallID == "" {
			return SchemaError(fmt.Sprintf("doors[%d].wall_id", i), "door %s must reference a wall", door.ID)
		}
		if door.Width <= 0 {
			return GeometryError("", door.WallID, "width", "door %s on wall %s has non-positive width", door.ID, door.WallID)
		}
		wall, ok := wallByID[door.WallID]
		if !ok {
			return GeometryError("", door.WallID, "wall_id", "door %s references unknown wall %s", door.ID, door.WallID)
		}
		if door.Start < wall.Start() || door.Start+door.Width > wall.End() {
			return GeometryError("", wall.ID, "opening", "door %s [%d,%d] is outside wall %s [%d,%d]",
				door.ID, door.Start, door.Start+door.Width, wall.ID, wall.Start(), wall.End())
		}
		if _, exists := doorByID[door.ID]; exists {
			return GeometryError("", wall.ID, "id", "duplicate door id %s", door.ID)
		}
		doorByID[door.ID] = door
		doorsByWall[door.WallID] = append(doorsByWall[door.WallID], door)
	}
	for wallID, doors := range doorsByWall {
		sort.Slice(doors, func(i, j int) bool { return doors[i].Start < doors[j].Start })
		for i := 1; i < len(doors); i++ {
			if doors[i].Start < doors[i-1].Start+doors[i-1].Width {
				return GeometryError("", wallID, "opening", "doors %s and %s overlap on wall %s", doors[i-1].ID, doors[i].ID, wallID)
			}
		}
	}

	sourceIDs := make(map[string]struct{}, len(p.Sources))
	for i, source := range p.Sources {
		if source.ID == "" {
			return SchemaError(fmt.Sprintf("sources[%d].id", i), "source id is required")
		}
		if _, exists := sourceIDs[source.ID]; exists {
			return GeometryError("", "", "source_id", "duplicate source id %s", source.ID)
		}
		sourceIDs[source.ID] = struct{}{}
		if !p.Bounds.ContainsInterior(source.X, source.Y) {
			e := GeometryError("", "", "source_position", "source %s lies outside level bounds", source.ID)
			e.SourceID = source.ID
			return e
		}
		if _, ok := RoomForPoint(p.Rooms, source.X, source.Y); !ok {
			e := GeometryError("", "", "source_position", "source %s is not inside exactly one room", source.ID)
			e.SourceID = source.ID
			return e
		}
	}

	listenerIDs := make(map[string]struct{}, len(p.Listeners))
	for i, listener := range p.Listeners {
		if listener.ID == "" {
			return SchemaError(fmt.Sprintf("listeners[%d].id", i), "listener id is required")
		}
		if _, exists := listenerIDs[listener.ID]; exists {
			return GeometryError("", "", "listener_id", "duplicate listener id %s", listener.ID)
		}
		listenerIDs[listener.ID] = struct{}{}
		if !p.Bounds.ContainsInterior(listener.X, listener.Y) {
			e := GeometryError("", "", "listener_position", "listener %s lies outside level bounds", listener.ID)
			e.ListenerID = listener.ID
			return e
		}
		if _, ok := RoomForPoint(p.Rooms, listener.X, listener.Y); !ok {
			e := GeometryError("", "", "listener_position", "listener %s is not inside exactly one room", listener.ID)
			e.ListenerID = listener.ID
			return e
		}
	}

	if opts.RequireTruth {
		if p.Truth == nil {
			return SchemaError("truth", "truth checklist is required")
		}
		if err := validateTruth(p); err != nil {
			return err
		}
	} else if p.Truth != nil {
		if err := validateTruth(p); err != nil {
			return err
		}
	}
	return nil
}

func validateTruth(p *Project) error {
	t := p.Truth
	if t.AlgorithmVersion == "" {
		return SchemaError("truth.algorithm_version", "truth algorithm version is required")
	}
	if t.ProjectFingerprint == "" {
		return SchemaError("truth.project_fingerprint", "truth project fingerprint is required")
	}
	if t.CurveSamples <= 0 {
		return SchemaError("truth.curve_samples", "truth curve sample count must be positive")
	}
	if t.CurveStepMicros <= 0 {
		return SchemaError("truth.curve_step_micros", "truth curve step must be positive")
	}
	sourceIDs := make(map[string]struct{}, len(p.Sources))
	for _, s := range p.Sources {
		sourceIDs[s.ID] = struct{}{}
	}
	listenerByID := make(map[string]Listener, len(p.Listeners))
	for _, l := range p.Listeners {
		listenerByID[l.ID] = l
	}
	roomByID := make(map[string]Room, len(p.Rooms))
	for _, room := range p.Rooms {
		roomByID[room.ID] = room
	}
	doorByID := make(map[string]Door, len(p.Doors))
	for _, door := range p.Doors {
		doorByID[door.ID] = door
	}
	seen := make(map[string]struct{}, len(t.Entries))
	for i, entry := range t.Entries {
		if entry.SourceID == "" || entry.ListenerID == "" || entry.ListenerRoomID == "" {
			return SchemaError(fmt.Sprintf("truth.entries[%d]", i), "truth pair must declare source, listener and listener room")
		}
		if _, ok := sourceIDs[entry.SourceID]; !ok {
			return SchemaError(fmt.Sprintf("truth.entries[%d].source_id", i), "unknown source %s", entry.SourceID)
		}
		listener, ok := listenerByID[entry.ListenerID]
		if !ok {
			return SchemaError(fmt.Sprintf("truth.entries[%d].listener_id", i), "unknown listener %s", entry.ListenerID)
		}
		room, ok := RoomForPoint(p.Rooms, listener.X, listener.Y)
		if !ok || room.ID != entry.ListenerRoomID {
			return SchemaError(fmt.Sprintf("truth.entries[%d].listener_room_id", i),
				"truth listener room %s does not match geometry for listener %s", entry.ListenerRoomID, entry.ListenerID)
		}
		if _, ok := roomByID[entry.ListenerRoomID]; !ok {
			return SchemaError(fmt.Sprintf("truth.entries[%d].listener_room_id", i), "unknown listener room %s", entry.ListenerRoomID)
		}
		key := PairID(entry.SourceID, entry.ListenerID)
		if _, ok := seen[key]; ok {
			return SchemaError(fmt.Sprintf("truth.entries[%d]", i), "duplicate truth pair %s", key)
		}
		seen[key] = struct{}{}
		if entry.Reachable {
			if len(entry.RoomChain) == 0 {
				return SchemaError(fmt.Sprintf("truth.entries[%d].room_chain", i), "reachable truth pair %s has no room chain", key)
			}
			if len(entry.DoorChain) != len(entry.RoomChain)-1 {
				return SchemaError(fmt.Sprintf("truth.entries[%d].door_chain", i), "truth pair %s has %d doors for %d rooms", key, len(entry.DoorChain), len(entry.RoomChain))
			}
			if len(entry.CurveDBMicro) != t.CurveSamples {
				return SchemaError(fmt.Sprintf("truth.entries[%d].curve_db_micro", i), "truth pair %s has %d curve samples, want %d", key, len(entry.CurveDBMicro), t.CurveSamples)
			}
			if len(entry.Segments) != len(entry.RoomChain) {
				return SchemaError(fmt.Sprintf("truth.entries[%d].segments", i), "truth pair %s has %d segments, want %d", key, len(entry.Segments), len(entry.RoomChain))
			}
			for j, roomID := range entry.RoomChain {
				if _, ok := roomByID[roomID]; !ok {
					return SchemaError(fmt.Sprintf("truth.entries[%d].room_chain[%d]", i, j), "unknown room %s", roomID)
				}
				if entry.Segments[j].RoomID != roomID {
					return SchemaError(fmt.Sprintf("truth.entries[%d].segments[%d].room_id", i, j), "segment room %s does not match chain room %s", entry.Segments[j].RoomID, roomID)
				}
			}
			for j, doorID := range entry.DoorChain {
				if _, ok := doorByID[doorID]; !ok {
					return SchemaError(fmt.Sprintf("truth.entries[%d].door_chain[%d]", i, j), "unknown door %s", doorID)
				}
			}
		} else if len(entry.RoomChain) != 0 || len(entry.DoorChain) != 0 || len(entry.Segments) != 0 {
			return SchemaError(fmt.Sprintf("truth.entries[%d]", i), "unreachable truth pair %s must not carry a path", key)
		}
	}
	if len(seen) != len(p.Sources)*len(p.Listeners) {
		return SchemaError("truth.entries", "truth has %d pairs, want %d", len(seen), len(p.Sources)*len(p.Listeners))
	}
	fingerprint, err := ProjectFingerprint(p)
	if err != nil {
		return err
	}
	if t.ProjectFingerprint != fingerprint {
		return SchemaError("truth.project_fingerprint", "truth fingerprint does not match project geometry/parameters")
	}
	return nil
}

func rectInside(inner, outer Rect) bool {
	return inner.X >= outer.X && inner.Y >= outer.Y &&
		inner.Right() <= outer.Right() && inner.Bottom() <= outer.Bottom()
}

func rectContainsPointInclusive(r Rect, x, y int) bool {
	return x >= r.X && x <= r.Right() && y >= r.Y && y <= r.Bottom()
}

func wallIsSharedEdge(a, b Rect, wall Wall) bool {
	if wall.Vertical() {
		x := wall.X1
		if !((x == a.Right() && x == b.X) || (x == a.X && x == b.Right())) {
			return false
		}
		lo := max(wall.Start(), max(a.Y, b.Y))
		hi := min(wall.End(), min(a.Bottom(), b.Bottom()))
		return hi > lo && wall.Start() >= lo && wall.End() <= hi
	}
	y := wall.Y1
	if !((y == a.Bottom() && y == b.Y) || (y == a.Y && y == b.Bottom())) {
		return false
	}
	lo := max(wall.Start(), max(a.X, b.X))
	hi := min(wall.End(), min(a.Right(), b.Right()))
	return hi > lo && wall.Start() >= lo && wall.End() <= hi
}
