package acoustic

import (
	"fmt"
	"math"
)

func (p *Project) Validate() error {
	if p == nil {
		return &Error{Code: CodeSchema, Op: "validate", Message: "project is nil"}
	}
	if err := validateHeader(p); err != nil {
		return err
	}
	if err := validateRooms(p); err != nil {
		return err
	}
	if err := validateWalls(p); err != nil {
		return err
	}
	if err := validateOpenings(p); err != nil {
		return err
	}
	if err := validateReflectors(p); err != nil {
		return err
	}
	if err := validateSourcesAndListeners(p); err != nil {
		return err
	}
	if err := validateTruth(p); err != nil {
		return err
	}
	return nil
}

func validateHeader(p *Project) error {
	if p.SchemaVersion != SchemaVersionCurrent {
		return &Error{Code: CodeSchema, Op: "validate", Field: "schemaVersion", Message: fmt.Sprintf("unsupported schema version %d", p.SchemaVersion)}
	}
	if p.ParamsVersion <= 0 {
		return &Error{Code: CodeSchema, Op: "validate", Field: "paramsVersion", Message: "must be positive"}
	}
	if p.AlgorithmVersion == "" {
		return &Error{Code: CodeSchema, Op: "validate", Field: "algorithmVersion", Message: "must not be empty"}
	}
	if p.Tolerance.CoordinateTicks < 0 || p.Tolerance.PathTicks < 0 || p.Tolerance.IncidenceCos < 0 || p.Tolerance.SPLDB < 0 {
		return &Error{Code: CodeSchema, Op: "validate", Field: "tolerance", Message: "tolerances must be non-negative"}
	}
	return nil
}

func validateRooms(p *Project) error {
	if len(p.Rooms) == 0 {
		return &Error{Code: CodeSchema, Op: "validate", Field: "rooms", Message: "at least one room is required"}
	}
	seen := make(map[int]struct{}, len(p.Rooms))
	for i, room := range p.Rooms {
		if room.ID <= 0 {
			return &Error{Code: CodeSchema, Op: "validate", Field: fmt.Sprintf("rooms[%d].id", i), Message: "room id must be positive"}
		}
		if _, ok := seen[room.ID]; ok {
			return &Error{Code: CodeSchema, Op: "validate", Field: "rooms", RoomIDs: []int{room.ID}, Message: "duplicate room id"}
		}
		seen[room.ID] = struct{}{}
		if room.Rect.Width() <= 0 || room.Rect.Height() <= 0 {
			return &Error{Code: CodeGeometry, Op: "validate", Field: fmt.Sprintf("rooms[%d].rect", i), RoomIDs: []int{room.ID}, Message: "rectangle must have positive width and height"}
		}
		if err := validateRectBounds(room.Rect); err != nil {
			return &Error{Code: CodeGeometry, Op: "validate", Field: fmt.Sprintf("rooms[%d].rect", i), RoomIDs: []int{room.ID}, Message: "coordinate is outside the declared fixed-point range"}
		}
	}
	for i := 0; i < len(p.Rooms); i++ {
		for j := i + 1; j < len(p.Rooms); j++ {
			if rectsOverlapInterior(p.Rooms[i].Rect, p.Rooms[j].Rect) {
				return &Error{
					Code:    CodeGeometry,
					Op:      "validate",
					RoomIDs: []int{p.Rooms[i].ID, p.Rooms[j].ID},
					Message: "room rectangles overlap with positive area",
				}
			}
			if rectsTouchOnlyAtPoint(p.Rooms[i].Rect, p.Rooms[j].Rect) {
				return &Error{
					Code:    CodeGeometry,
					Op:      "validate",
					RoomIDs: []int{p.Rooms[i].ID, p.Rooms[j].ID},
					Message: "room rectangles may only touch along a shared edge",
				}
			}
		}
	}
	return nil
}

func validateWalls(p *Project) error {
	seen := make(map[int]struct{}, len(p.Walls))
	for i, wall := range p.Walls {
		if wall.ID <= 0 {
			return &Error{Code: CodeSchema, Op: "validate", Field: fmt.Sprintf("walls[%d].id", i), Message: "wall id must be positive"}
		}
		if _, ok := seen[wall.ID]; ok {
			return &Error{Code: CodeSchema, Op: "validate", Field: "walls", WallIDs: []int{wall.ID}, Message: "duplicate wall id"}
		}
		seen[wall.ID] = struct{}{}
		if wall.RoomA == wall.RoomB {
			return &Error{Code: CodeGeometry, Op: "validate", RoomIDs: []int{wall.RoomA}, WallIDs: []int{wall.ID}, Message: "shared wall must separate two different rooms"}
		}
		roomA, okA := p.roomByID(wall.RoomA)
		roomB, okB := p.roomByID(wall.RoomB)
		if !okA || !okB {
			ids := []int{wall.RoomA, wall.RoomB}
			return &Error{Code: CodeSchema, Op: "validate", RoomIDs: ids, WallIDs: []int{wall.ID}, Message: "wall references a missing room"}
		}
		if err := validatePointBounds(wall.A); err != nil {
			return &Error{Code: CodeGeometry, Op: "validate", RoomIDs: []int{wall.RoomA, wall.RoomB}, WallIDs: []int{wall.ID}, Message: "wall coordinate is outside the fixed-point range"}
		}
		if err := validatePointBounds(wall.B); err != nil {
			return &Error{Code: CodeGeometry, Op: "validate", RoomIDs: []int{wall.RoomA, wall.RoomB}, WallIDs: []int{wall.ID}, Message: "wall coordinate is outside the fixed-point range"}
		}
		if _, ok := wallSharedBoundary(*roomA, *roomB, wall); !ok {
			return &Error{Code: CodeGeometry, Op: "validate", RoomIDs: []int{wall.RoomA, wall.RoomB}, WallIDs: []int{wall.ID}, Message: "wall must be a positive segment on the shared edge of its two rooms"}
		}
	}
	return nil
}

func validateOpenings(p *Project) error {
	seen := make(map[int]struct{}, len(p.Openings))
	byWall := make(map[int][]Opening)
	for i, opening := range p.Openings {
		if opening.ID <= 0 {
			return &Error{Code: CodeSchema, Op: "validate", Field: fmt.Sprintf("openings[%d].id", i), Message: "opening id must be positive"}
		}
		if _, ok := seen[opening.ID]; ok {
			return &Error{Code: CodeSchema, Op: "validate", Field: "openings", OpeningIDs: []int{opening.ID}, Message: "duplicate opening id"}
		}
		seen[opening.ID] = struct{}{}
		wall, ok := p.wallByID(opening.WallID)
		if !ok {
			return &Error{Code: CodeSchema, Op: "validate", OpeningIDs: []int{opening.ID}, Message: "opening references a missing wall"}
		}
		length := wallLength(*wall)
		if opening.Width <= 0 {
			return &Error{Code: CodeGeometry, Op: "validate", RoomIDs: []int{wall.RoomA, wall.RoomB}, WallIDs: []int{wall.ID}, OpeningIDs: []int{opening.ID}, Message: "opening width must be positive"}
		}
		if opening.Start < 0 || opening.Start+opening.Width > length {
			return &Error{Code: CodeGeometry, Op: "validate", RoomIDs: []int{wall.RoomA, wall.RoomB}, WallIDs: []int{wall.ID}, OpeningIDs: []int{opening.ID}, Message: "opening lies outside its wall segment"}
		}
		byWall[wall.ID] = append(byWall[wall.ID], opening)
	}
	for wallID, openings := range byWall {
		for i := 0; i < len(openings); i++ {
			for j := i + 1; j < len(openings); j++ {
				a0, a1 := openings[i].Start, openings[i].Start+openings[i].Width
				b0, b1 := openings[j].Start, openings[j].Start+openings[j].Width
				if maxInt64(a0, b0) < minInt64(a1, b1) {
					wall, _ := p.wallByID(wallID)
					return &Error{Code: CodeGeometry, Op: "validate", RoomIDs: []int{wall.RoomA, wall.RoomB}, WallIDs: []int{wallID}, OpeningIDs: []int{openings[i].ID, openings[j].ID}, Message: "openings overlap on the same wall"}
				}
			}
		}
	}
	return nil
}

func validateReflectors(p *Project) error {
	seen := make(map[int]struct{}, len(p.Reflectors))
	for i, reflector := range p.Reflectors {
		if reflector.ID <= 0 {
			return &Error{Code: CodeSchema, Op: "validate", Field: fmt.Sprintf("reflectors[%d].id", i), Message: "reflector id must be positive"}
		}
		if _, ok := seen[reflector.ID]; ok {
			return &Error{Code: CodeSchema, Op: "validate", Field: "reflectors", ReflectorIDs: []int{reflector.ID}, Message: "duplicate reflector id"}
		}
		seen[reflector.ID] = struct{}{}
		room, ok := p.roomByID(reflector.RoomID)
		if !ok {
			return &Error{Code: CodeSchema, Op: "validate", ReflectorIDs: []int{reflector.ID}, Message: "reflector references a missing room"}
		}
		if !room.Rect.ContainsClosed(reflector.A) || !room.Rect.ContainsClosed(reflector.B) {
			return &Error{Code: CodeGeometry, Op: "validate", RoomIDs: []int{room.ID}, ReflectorIDs: []int{reflector.ID}, Message: "reflector endpoints must lie inside the room"}
		}
		if reflector.A == reflector.B {
			return &Error{Code: CodeGeometry, Op: "validate", RoomIDs: []int{room.ID}, ReflectorIDs: []int{reflector.ID}, Message: "reflector segment must have positive length"}
		}
		if err := validatePointBounds(reflector.A); err != nil {
			return &Error{Code: CodeGeometry, Op: "validate", RoomIDs: []int{room.ID}, ReflectorIDs: []int{reflector.ID}, Message: "reflector coordinate is outside the fixed-point range"}
		}
		if err := validatePointBounds(reflector.B); err != nil {
			return &Error{Code: CodeGeometry, Op: "validate", RoomIDs: []int{room.ID}, ReflectorIDs: []int{reflector.ID}, Message: "reflector coordinate is outside the fixed-point range"}
		}
		if err := validateUnitNormal(reflector); err != nil {
			err.RoomIDs = []int{room.ID}
			err.ReflectorIDs = []int{reflector.ID}
			return err
		}
		if math.IsNaN(reflector.Absorption) || math.IsInf(reflector.Absorption, 0) || reflector.Absorption < 0 || reflector.Absorption > 1 {
			return &Error{Code: CodeSchema, Op: "validate", RoomIDs: []int{room.ID}, ReflectorIDs: []int{reflector.ID}, Field: fmt.Sprintf("reflectors[%d].absorption", i), Message: "absorption must be in [0,1]"}
		}
	}
	return nil
}

func validateUnitNormal(reflector Reflector) *Error {
	if absInt64(reflector.NX) > 2*FixedScale || absInt64(reflector.NY) > 2*FixedScale {
		return &Error{Code: CodeGeometry, Op: "validate", Message: "reflector normal components are outside the fixed-point range"}
	}
	scaleSquared := FixedScale * FixedScale
	q := reflector.NX*reflector.NX + reflector.NY*reflector.NY
	if absInt64(q-scaleSquared) > 4*FixedScale {
		return &Error{Code: CodeGeometry, Op: "validate", Message: "reflector normal must be a unit direction at the fixed-point scale"}
	}
	line := pointSub(reflector.B, reflector.A)
	dot := reflector.NX*line.X + reflector.NY*line.Y
	lineLength := math.Hypot(float64(line.X), float64(line.Y))
	if math.Abs(float64(dot)) > 3*lineLength {
		return &Error{Code: CodeGeometry, Op: "validate", Message: "reflector normal must be perpendicular to its surface"}
	}
	return nil
}

func validateSourcesAndListeners(p *Project) error {
	sourceSeen := make(map[int]struct{}, len(p.Sources))
	for i, source := range p.Sources {
		if source.ID <= 0 {
			return &Error{Code: CodeSchema, Op: "validate", Field: fmt.Sprintf("sources[%d].id", i), Message: "source id must be positive"}
		}
		if _, ok := sourceSeen[source.ID]; ok {
			return &Error{Code: CodeSchema, Op: "validate", Field: "sources", SourceIDs: []int{source.ID}, Message: "duplicate source id"}
		}
		sourceSeen[source.ID] = struct{}{}
		room, ok := p.roomByID(source.RoomID)
		if !ok {
			return &Error{Code: CodeSchema, Op: "validate", SourceIDs: []int{source.ID}, Message: "source references a missing room"}
		}
		if !room.Rect.ContainsInterior(source.Pos) {
			return &Error{Code: CodeGeometry, Op: "validate", RoomIDs: []int{room.ID}, SourceIDs: []int{source.ID}, Message: "source must lie strictly inside its room"}
		}
		if err := validatePointBounds(source.Pos); err != nil {
			return &Error{Code: CodeGeometry, Op: "validate", RoomIDs: []int{room.ID}, SourceIDs: []int{source.ID}, Message: "source coordinate is outside the fixed-point range"}
		}
		if math.IsNaN(source.LevelDB) || math.IsInf(source.LevelDB, 0) {
			return &Error{Code: CodeSchema, Op: "validate", RoomIDs: []int{room.ID}, SourceIDs: []int{source.ID}, Field: fmt.Sprintf("sources[%d].levelDb", i), Message: "source level must be finite"}
		}
	}

	listenerSeen := make(map[int]struct{}, len(p.Listeners))
	for i, listener := range p.Listeners {
		if listener.ID <= 0 {
			return &Error{Code: CodeSchema, Op: "validate", Field: fmt.Sprintf("listeners[%d].id", i), Message: "listener id must be positive"}
		}
		if _, ok := listenerSeen[listener.ID]; ok {
			return &Error{Code: CodeSchema, Op: "validate", Field: "listeners", ListenerIDs: []int{listener.ID}, Message: "duplicate listener id"}
		}
		listenerSeen[listener.ID] = struct{}{}
		rooms := p.roomsContainingInterior(listener.Pos)
		if len(rooms) != 1 || rooms[0].ID != listener.RoomID {
			ids := make([]int, 0, len(rooms)+1)
			for _, room := range rooms {
				ids = append(ids, room.ID)
			}
			if len(ids) == 0 {
				ids = append(ids, listener.RoomID)
			}
			return &Error{Code: CodeGeometry, Op: "validate", RoomIDs: ids, ListenerIDs: []int{listener.ID}, Message: "listener must lie strictly inside exactly one declared room"}
		}
		if err := validatePointBounds(listener.Pos); err != nil {
			return &Error{Code: CodeGeometry, Op: "validate", RoomIDs: []int{listener.RoomID}, ListenerIDs: []int{listener.ID}, Message: "listener coordinate is outside the fixed-point range"}
		}
	}
	return nil
}

func validateTruth(p *Project) error {
	if len(p.Truth) != len(p.Listeners) {
		return &Error{Code: CodeSchema, Op: "validate", Field: "truth", Message: "truth must contain exactly one entry per listener"}
	}
	truthSeen := make(map[int]struct{}, len(p.Truth))
	for i, truth := range p.Truth {
		if _, ok := truthSeen[truth.ListenerID]; ok {
			return &Error{Code: CodeSchema, Op: "validate", Field: fmt.Sprintf("truth[%d].listenerId", i), ListenerIDs: []int{truth.ListenerID}, Message: "duplicate listener truth"}
		}
		truthSeen[truth.ListenerID] = struct{}{}
		listener, ok := p.listenerByID(truth.ListenerID)
		if !ok {
			return &Error{Code: CodeSchema, Op: "validate", ListenerIDs: []int{truth.ListenerID}, Message: "truth references a missing listener"}
		}
		if truth.RoomID != listener.RoomID {
			return &Error{Code: CodeSchema, Op: "validate", RoomIDs: []int{truth.RoomID, listener.RoomID}, ListenerIDs: []int{listener.ID}, Message: "truth room does not match listener room"}
		}
		if len(truth.Sources) != len(p.Sources) {
			return &Error{Code: CodeSchema, Op: "validate", ListenerIDs: []int{listener.ID}, Message: "truth must contain exactly one entry per source"}
		}
		sourceSeen := make(map[int]struct{}, len(truth.Sources))
		aggregate := 0
		for j, sourceTruth := range truth.Sources {
			if _, ok := sourceSeen[sourceTruth.SourceID]; ok {
				return &Error{Code: CodeSchema, Op: "validate", ListenerIDs: []int{listener.ID}, SourceIDs: []int{sourceTruth.SourceID}, Message: "duplicate source truth"}
			}
			sourceSeen[sourceTruth.SourceID] = struct{}{}
			if _, ok := p.sourceByID(sourceTruth.SourceID); !ok {
				return &Error{Code: CodeSchema, Op: "validate", ListenerIDs: []int{listener.ID}, SourceIDs: []int{sourceTruth.SourceID}, Message: "truth references a missing source"}
			}
			if sourceTruth.ValidPathCount < 0 || sourceTruth.ValidPathCount != len(sourceTruth.Paths) {
				return &Error{Code: CodeSchema, Op: "validate", ListenerIDs: []int{listener.ID}, SourceIDs: []int{sourceTruth.SourceID}, Field: fmt.Sprintf("truth[%d].sources[%d].validPathCount", i, j), Message: "validPathCount must equal the number of truth paths"}
			}
			if sourceTruth.ChainLengthTicks < 0 {
				return &Error{Code: CodeSchema, Op: "validate", ListenerIDs: []int{listener.ID}, SourceIDs: []int{sourceTruth.SourceID}, Message: "chain length must be non-negative"}
			}
			pathSeen := make(map[[2]int]struct{}, len(sourceTruth.Paths))
			for k, path := range sourceTruth.Paths {
				if path.SourceID != sourceTruth.SourceID {
					return &Error{Code: CodeSchema, Op: "validate", ListenerIDs: []int{listener.ID}, SourceIDs: []int{sourceTruth.SourceID}, Field: fmt.Sprintf("truth[%d].sources[%d].paths[%d].sourceId", i, j, k), Message: "path source does not match its source truth"}
				}
				reflector, ok := p.reflectorByID(path.ReflectorID)
				if !ok {
					return &Error{Code: CodeSchema, Op: "validate", ListenerIDs: []int{listener.ID}, ReflectorIDs: []int{path.ReflectorID}, Message: "truth path references a missing reflector"}
				}
				if reflector.RoomID != listener.RoomID {
					return &Error{Code: CodeSchema, Op: "validate", RoomIDs: []int{reflector.RoomID, listener.RoomID}, ListenerIDs: []int{listener.ID}, ReflectorIDs: []int{reflector.ID}, Message: "truth reflector must be in the listener room"}
				}
				if _, ok := pathSeen[[2]int{path.SourceID, path.ReflectorID}]; ok {
					return &Error{Code: CodeSchema, Op: "validate", ListenerIDs: []int{listener.ID}, SourceIDs: []int{path.SourceID}, ReflectorIDs: []int{path.ReflectorID}, Message: "duplicate truth path"}
				}
				pathSeen[[2]int{path.SourceID, path.ReflectorID}] = struct{}{}
				if path.PathLengthTicks <= 0 || path.IncidenceCos < 0 || path.IncidenceCos > FixedScale {
					return &Error{Code: CodeSchema, Op: "validate", ListenerIDs: []int{listener.ID}, SourceIDs: []int{path.SourceID}, ReflectorIDs: []int{path.ReflectorID}, Message: "invalid truth path measurement"}
				}
				if math.IsNaN(path.SPLDB) || math.IsInf(path.SPLDB, 0) {
					return &Error{Code: CodeSchema, Op: "validate", ListenerIDs: []int{listener.ID}, SourceIDs: []int{path.SourceID}, ReflectorIDs: []int{path.ReflectorID}, Message: "truth SPL must be finite"}
				}
			}
			aggregate += sourceTruth.ValidPathCount
		}
		if truth.ValidPathCount != aggregate {
			return &Error{Code: CodeSchema, Op: "validate", ListenerIDs: []int{listener.ID}, Message: "listener validPathCount must equal source totals"}
		}
	}
	for _, listener := range p.Listeners {
		if _, ok := truthSeen[listener.ID]; !ok {
			return &Error{Code: CodeSchema, Op: "validate", ListenerIDs: []int{listener.ID}, Message: "missing listener truth"}
		}
	}
	return nil
}

func (p *Project) roomsContainingInterior(point Point) []Room {
	var rooms []Room
	for _, room := range p.Rooms {
		if room.Rect.ContainsInterior(point) {
			rooms = append(rooms, room)
		}
	}
	return rooms
}

func validateRectBounds(rect Rect) error {
	for _, value := range []Fixed{rect.MinX, rect.MinY, rect.MaxX, rect.MaxY} {
		if absInt64(value) > MaxAbsFixedCoordinate {
			return fmt.Errorf("coordinate out of range")
		}
	}
	return nil
}

func validatePointBounds(point Point) error {
	if absInt64(point.X) > MaxAbsFixedCoordinate || absInt64(point.Y) > MaxAbsFixedCoordinate {
		return fmt.Errorf("coordinate out of range")
	}
	return nil
}
