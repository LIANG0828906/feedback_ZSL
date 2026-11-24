package levelio

import (
	"encoding/json"
	"fmt"
	"math"
	"os"
	"strings"

	"roomreverb/internal/geom"
	"roomreverb/internal/model"
)

func LoadFile(path string) (*model.Level, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, model.NewError(model.CodeSchema, path, err.Error())
	}
	return Load(data)
}

func Load(data []byte) (*model.Level, error) {
	if err := requireSchema(data); err != nil {
		return nil, err
	}
	var level model.Level
	if err := json.Unmarshal(data, &level); err != nil {
		return nil, model.NewError(model.CodeSchema, "json", err.Error())
	}
	if err := Validate(&level); err != nil {
		return nil, err
	}
	return &level, nil
}

func requireSchema(data []byte) error {
	var top map[string]json.RawMessage
	if err := json.Unmarshal(data, &top); err != nil {
		return model.NewError(model.CodeSchema, "json", err.Error())
	}
	for _, key := range []string{"schema_version", "rooms", "walls", "doors", "sources", "listeners", "truth"} {
		if _, ok := top[key]; !ok {
			return model.NewError(model.CodeSchema, key, "missing required top-level field")
		}
	}

	var envelope struct {
		Truth json.RawMessage `json:"truth"`
	}
	_ = json.Unmarshal(data, &envelope)
	var truth map[string]json.RawMessage
	if err := json.Unmarshal(envelope.Truth, &truth); err != nil {
		return model.NewError(model.CodeSchema, "truth", "truth must be an object")
	}
	for _, key := range []string{"listener_rooms", "paths"} {
		if _, ok := truth[key]; !ok {
			return model.NewError(model.CodeSchema, "truth."+key, "missing required truth field")
		}
	}

	checks := []struct {
		arrayKey string
		fields   []string
		nested   map[string][]string
	}{
		{"rooms", []string{"id", "rect", "rt60_seconds", "early_reflection_density", "reverb_gain"}, map[string][]string{"rect": {"min", "max"}, "rect.min": {"x", "y"}, "rect.max": {"x", "y"}}},
		{"walls", []string{"id", "a", "b", "room_a_id"}, map[string][]string{"a": {"x", "y"}, "b": {"x", "y"}}},
		{"doors", []string{"id", "wall_id", "a", "b", "kind"}, map[string][]string{"a": {"x", "y"}, "b": {"x", "y"}}},
		{"sources", []string{"id", "pos", "level_db"}, map[string][]string{"pos": {"x", "y"}}},
		{"listeners", []string{"id", "pos"}, map[string][]string{"pos": {"x", "y"}}},
	}
	for _, check := range checks {
		items, err := rawArray(top[check.arrayKey])
		if err != nil {
			return err
		}
		for i, item := range items {
			ref := fmt.Sprintf("%s[%d]", check.arrayKey, i)
			obj, err := rawObject(item, ref)
			if err != nil {
				return err
			}
			if err := requireFields(obj, check.fields, ref); err != nil {
				return err
			}
			for nested, fields := range check.nested {
				nestedRaw, pathErr := rawPath(obj, nested)
				if pathErr != nil {
					return model.NewError(model.CodeSchema, ref+"."+nested, "missing nested field")
				}
				nestedObj, err := rawObject(nestedRaw, ref+"."+nested)
				if err != nil {
					return err
				}
				if err := requireFields(nestedObj, fields, ref+"."+nested); err != nil {
					return err
				}
			}
		}
	}

	listenerTruths, err := rawArray(truth["listener_rooms"])
	if err != nil {
		return model.NewError(model.CodeSchema, "truth.listener_rooms", err.Error())
	}
	for i, raw := range listenerTruths {
		obj, err := rawObject(raw, fmt.Sprintf("truth.listener_rooms[%d]", i))
		if err != nil {
			return err
		}
		if err := requireFields(obj, []string{"listener_id", "room_id"}, fmt.Sprintf("truth.listener_rooms[%d]", i)); err != nil {
			return err
		}
	}
	pathTruths, err := rawArray(truth["paths"])
	if err != nil {
		return model.NewError(model.CodeSchema, "truth.paths", err.Error())
	}
	for i, raw := range pathTruths {
		obj, err := rawObject(raw, fmt.Sprintf("truth.paths[%d]", i))
		if err != nil {
			return err
		}
		if err := requireFields(obj, []string{"source_id", "listener_id", "room_chain", "length", "curve_db", "spl_db"}, fmt.Sprintf("truth.paths[%d]", i)); err != nil {
			return err
		}
	}
	return nil
}

func rawPath(obj map[string]json.RawMessage, path string) (json.RawMessage, error) {
	parts := strings.Split(path, ".")
	var current json.RawMessage
	currentObj := obj
	for i, part := range parts {
		raw, ok := currentObj[part]
		if !ok {
			return nil, fmt.Errorf("missing %s", path)
		}
		if i == len(parts)-1 {
			return raw, nil
		}
		nextObj, err := rawObject(raw, path)
		if err != nil {
			return nil, err
		}
		currentObj = nextObj
		current = raw
	}
	return current, nil
}

func rawArray(raw json.RawMessage) ([]json.RawMessage, error) {
	var items []json.RawMessage
	if err := json.Unmarshal(raw, &items); err != nil {
		return nil, model.NewError(model.CodeSchema, "array", err.Error())
	}
	return items, nil
}

func rawObject(raw json.RawMessage, ref string) (map[string]json.RawMessage, error) {
	var obj map[string]json.RawMessage
	if err := json.Unmarshal(raw, &obj); err != nil {
		return nil, model.NewError(model.CodeSchema, ref, "expected object")
	}
	return obj, nil
}

func requireFields(obj map[string]json.RawMessage, fields []string, ref string) error {
	for _, field := range fields {
		raw, ok := obj[field]
		if !ok || string(raw) == "null" {
			return model.NewError(model.CodeSchema, ref+"."+field, "missing required field")
		}
	}
	return nil
}

func Validate(level *model.Level) error {
	if level == nil {
		return model.NewError(model.CodeSchema, "level", "nil level")
	}
	if level.UnitsPerMeter <= 0 {
		level.UnitsPerMeter = 20
	}
	if level.SampleCount <= 0 {
		level.SampleCount = 32
	}
	if level.SampleDT <= 0 {
		level.SampleDT = 0.02
	}

	roomByID := map[string]model.Room{}
	for i, room := range level.Rooms {
		ref := room.ID
		if ref == "" {
			ref = fmt.Sprintf("rooms[%d]", i)
		}
		if _, exists := roomByID[room.ID]; exists {
			return model.NewError(model.CodeSchema, ref, "duplicate room id")
		}
		if room.Rect.Min.X >= room.Rect.Max.X || room.Rect.Min.Y >= room.Rect.Max.Y {
			return model.NewError(model.CodeGeom, ref, "room rectangle must have positive width and height")
		}
		if !finitePositive(room.RT60Seconds) || !finiteNonNegative(room.EarlyDensity) || !finitePositive(room.ReverbGain) {
			return model.NewError(model.CodeSchema, ref, "invalid reverb parameter")
		}
		if room.OcclusionPerDoor == 0 {
			room.OcclusionPerDoor = 1.2
		}
		roomByID[room.ID] = room
	}

	for i := 0; i < len(level.Rooms); i++ {
		for j := i + 1; j < len(level.Rooms); j++ {
			a, b := level.Rooms[i], level.Rooms[j]
			if geom.RectsOverlapInterior(a.Rect, b.Rect) {
				return model.NewError(model.CodeGeom, a.ID+"/"+b.ID, "room interiors overlap; rectangles may only share an edge")
			}
		}
	}

	wallByID := map[string]model.Wall{}
	for i, wall := range level.Walls {
		ref := wall.ID
		if ref == "" {
			ref = fmt.Sprintf("walls[%d]", i)
		}
		if _, exists := wallByID[wall.ID]; exists {
			return model.NewError(model.CodeSchema, ref, "duplicate wall id")
		}
		if wall.A == wall.B || (wall.A.X != wall.B.X && wall.A.Y != wall.B.Y) {
			return model.NewError(model.CodeGeom, ref, "wall must be a non-empty axis-aligned segment")
		}
		if wall.RoomAID == "" {
			return model.NewError(model.CodeSchema, ref+".room_a_id", "missing wall room")
		}
		roomA, ok := roomByID[wall.RoomAID]
		if !ok {
			return model.NewError(model.CodeSchema, ref+".room_a_id", "wall references unknown room")
		}
		if !geom.SegmentOnAxisAlignedBoundary(wall.A, wall.B, roomA.Rect) {
			return model.NewError(model.CodeGeom, ref, "wall segment is not on boundary of room "+wall.RoomAID)
		}
		if wall.RoomBID != "" {
			roomB, ok := roomByID[wall.RoomBID]
			if !ok {
				return model.NewError(model.CodeSchema, ref+".room_b_id", "wall references unknown room")
			}
			if !geom.SegmentOnAxisAlignedBoundary(wall.A, wall.B, roomB.Rect) {
				return model.NewError(model.CodeGeom, ref, "shared wall is not on boundary of room "+wall.RoomBID)
			}
		}
		wallByID[wall.ID] = wall
	}

	doorIDs := map[string]bool{}
	for i, door := range level.Doors {
		ref := door.ID
		if ref == "" {
			ref = fmt.Sprintf("doors[%d]", i)
		}
		if doorIDs[door.ID] {
			return model.NewError(model.CodeSchema, ref, "duplicate door id")
		}
		doorIDs[door.ID] = true
		wall, ok := wallByID[door.WallID]
		if !ok {
			return model.NewError(model.CodeSchema, ref+".wall_id", "door references unknown wall")
		}
		if door.A == door.B || door.Width() <= 0 {
			return model.NewError(model.CodeGeom, ref, "door opening must have positive width")
		}
		if door.A.X != door.B.X && door.A.Y != door.B.Y {
			return model.NewError(model.CodeGeom, ref, "door must be axis aligned")
		}
		if !geom.SegmentContains(wall.A, wall.B, door.A, door.B) {
			return model.NewError(model.CodeGeom, wall.ID, "door "+door.ID+" lies outside its wall segment")
		}
	}

	bounds := level.Bounds
	if bounds.Min == (model.Point{}) && bounds.Max == (model.Rect{}).Max {
		// The zero-value comparison above is intentionally explicit for readability.
	}
	if bounds.Max.X <= bounds.Min.X || bounds.Max.Y <= bounds.Min.Y {
		minX, minY := math.MaxInt, math.MaxInt
		maxX, maxY := math.MinInt, math.MinInt
		for _, room := range level.Rooms {
			minX = min(minX, room.Rect.Min.X)
			minY = min(minY, room.Rect.Min.Y)
			maxX = max(maxX, room.Rect.Max.X)
			maxY = max(maxY, room.Rect.Max.Y)
		}
		bounds = model.Rect{Min: model.Point{X: minX, Y: minY}, Max: model.Point{X: maxX, Y: maxY}}
		level.Bounds = bounds
	}

	sourceIDs := map[string]bool{}
	for i, source := range level.Sources {
		ref := source.ID
		if ref == "" {
			ref = fmt.Sprintf("sources[%d]", i)
		}
		if sourceIDs[source.ID] {
			return model.NewError(model.CodeSchema, ref, "duplicate source id")
		}
		sourceIDs[source.ID] = true
		if !geom.PointInRect(source.Pos, bounds) {
			return model.NewError(model.CodeGeom, ref, "source coordinate is outside level bounds")
		}
		if geom.LocateRoom(level, source.Pos) == "" {
			return model.NewError(model.CodeGeom, ref, "source must be inside one room")
		}
	}

	listenerIDs := map[string]bool{}
	for i, listener := range level.Listeners {
		ref := listener.ID
		if ref == "" {
			ref = fmt.Sprintf("listeners[%d]", i)
		}
		if listenerIDs[listener.ID] {
			return model.NewError(model.CodeSchema, ref, "duplicate listener id")
		}
		listenerIDs[listener.ID] = true
		if !geom.PointInRect(listener.Pos, bounds) {
			return model.NewError(model.CodeGeom, ref, "listener coordinate is outside level bounds")
		}
		if geom.LocateRoom(level, listener.Pos) == "" {
			return model.NewError(model.CodeGeom, ref, "listener must be inside one room")
		}
	}

	counts := map[string]int{}
	for _, item := range level.Truth.ListenerRooms {
		counts[item.ListenerID]++
		if !listenerIDs[item.ListenerID] {
			return model.NewError(model.CodeSchema, "truth.listener_rooms", "truth references unknown listener "+item.ListenerID)
		}
		expected := geom.LocateRoom(level, mustListenerPos(level, item.ListenerID))
		if item.RoomID != expected {
			return model.NewError(model.CodeGeom, item.ListenerID, "truth listener room does not match geometry: expected "+expected)
		}
	}
	for _, listener := range level.Listeners {
		if counts[listener.ID] != 1 {
			return model.NewError(model.CodeSchema, "truth.listener_rooms:"+listener.ID, "each listener must appear exactly once")
		}
	}
	for _, path := range level.Truth.Paths {
		if !sourceIDs[path.SourceID] || !listenerIDs[path.ListenerID] {
			return model.NewError(model.CodeSchema, "truth.paths", "path truth references unknown source or listener")
		}
		if len(path.RoomChain) == 0 || len(path.CurveDB) == 0 {
			return model.NewError(model.CodeSchema, path.SourceID+"->"+path.ListenerID, "path truth is incomplete")
		}
		if !finiteNonNegative(path.Length) || !finiteFloat(path.SPLDB) {
			return model.NewError(model.CodeSchema, path.SourceID+"->"+path.ListenerID, "path truth has invalid numeric value")
		}
	}
	return nil
}

func mustListenerPos(level *model.Level, id string) model.Point {
	for _, listener := range level.Listeners {
		if listener.ID == id {
			return listener.Pos
		}
	}
	return model.Point{}
}

func finitePositive(v float64) bool    { return !math.IsNaN(v) && !math.IsInf(v, 0) && v > 0 }
func finiteNonNegative(v float64) bool { return !math.IsNaN(v) && !math.IsInf(v, 0) && v >= 0 }
func finiteFloat(v float64) bool       { return !math.IsNaN(v) && !math.IsInf(v, 0) }
