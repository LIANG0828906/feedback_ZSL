package wb

import (
	"encoding/json"
	"fmt"
)

func LoadSample(data []byte) (*Sample, *APIError) {
	var top map[string]json.RawMessage
	if err := json.Unmarshal(data, &top); err != nil {
		return nil, NewAPIError(CodeSchema, "sample", "", "invalid JSON: %v", err)
	}
	for _, key := range []string{"seed", "bounds", "precision", "rooms", "walls", "openings", "sources", "listeners", "truth"} {
		if _, ok := top[key]; !ok {
			return nil, NewAPIError(CodeSchema, "sample", key, "missing required field %q", key)
		}
	}

	if apiErr := requireRawKeys(top["bounds"], "bounds", "x1", "y1", "x2", "y2"); apiErr != nil {
		return nil, apiErr
	}
	if apiErr := requireRawKeys(top["precision"], "precision",
		"scale", "sampleCount", "dtMs", "toleranceDb", "maxParallel", "bufferSampleCap",
		"pathAttenPerUnitDb", "occlusionAttenDb", "earlyBoostMaxDb", "earlyWindowMs"); apiErr != nil {
		return nil, apiErr
	}
	if apiErr := requireRawKeys(top["truth"], "truth", "listenerRooms", "paths"); apiErr != nil {
		return nil, apiErr
	}
	var truthHead struct {
		Paths []json.RawMessage `json:"paths"`
	}
	if err := json.Unmarshal(top["truth"], &truthHead); err != nil {
		return nil, NewAPIError(CodeSchema, "truth", "", "invalid truth object: %v", err)
	}
	for i, rawPath := range truthHead.Paths {
		objectID := fmt.Sprintf("truth.paths[%d]", i)
		if apiErr := requireRawKeys(rawPath, objectID,
			"sourceId", "listenerId", "roomChain", "length", "expectedSplDb", "decaySamplesDb"); apiErr != nil {
			return nil, apiErr
		}
	}

	collections := []struct {
		key      string
		required []string
	}{
		{"rooms", []string{"id", "rect", "params"}},
		{"walls", []string{"id", "x1", "y1", "x2", "y2"}},
		{"openings", []string{"id", "wallId", "kind", "x1", "y1", "x2", "y2"}},
		{"sources", []string{"id", "x", "y", "powerDb"}},
		{"listeners", []string{"id", "x", "y"}},
	}
	for _, col := range collections {
		items, apiErr := rawItems(top[col.key], col.key)
		if apiErr != nil {
			return nil, apiErr
		}
		for i, item := range items {
			objID := fmt.Sprintf("%s[%d]", col.key, i)
			if apiErr := requireRawKeys(item, objID, col.required...); apiErr != nil {
				return nil, apiErr
			}
			if col.key == "rooms" {
				var room struct {
					ID     string          `json:"id"`
					Rect   json.RawMessage `json:"rect"`
					Params json.RawMessage `json:"params"`
				}
				_ = json.Unmarshal(item, &room)
				if apiErr := requireRawKeys(room.Rect, "room:"+room.ID+".rect", "x1", "y1", "x2", "y2"); apiErr != nil {
					return nil, apiErr
				}
				if apiErr := requireRawKeys(room.Params, "room:"+room.ID+".params",
					"rt60Ms", "earlyReflectionDensity", "gainDb"); apiErr != nil {
					return nil, apiErr
				}
			}
		}
	}

	var sample Sample
	if err := json.Unmarshal(data, &sample); err != nil {
		return nil, NewAPIError(CodeSchema, "sample", "", "schema decode failed: %v", err)
	}
	if apiErr := ValidateSample(&sample); apiErr != nil {
		return nil, apiErr
	}
	return &sample, nil
}

func rawItems(raw json.RawMessage, object string) ([]json.RawMessage, *APIError) {
	var items []json.RawMessage
	if err := json.Unmarshal(raw, &items); err != nil {
		return nil, NewAPIError(CodeSchema, object, "", "expected array: %v", err)
	}
	return items, nil
}

func requireRawKeys(raw json.RawMessage, object string, keys ...string) *APIError {
	var values map[string]json.RawMessage
	if err := json.Unmarshal(raw, &values); err != nil {
		return NewAPIError(CodeSchema, object, "", "expected object: %v", err)
	}
	for _, key := range keys {
		value, ok := values[key]
		if !ok || string(value) == "null" {
			return NewAPIError(CodeSchema, object, key, "missing required field %q", key)
		}
	}
	return nil
}

func ValidateSample(s *Sample) *APIError {
	if s == nil {
		return NewAPIError(CodeSchema, "sample", "", "nil sample")
	}
	if s.Bounds.X1 >= s.Bounds.X2 || s.Bounds.Y1 >= s.Bounds.Y2 {
		return NewAPIError(CodeGeom, "bounds", "", "level bounds must have positive width and height")
	}
	if apiErr := ValidatePrecision(s.Precision); apiErr != nil {
		return apiErr
	}

	roomByID := map[string]Room{}
	for _, room := range s.Rooms {
		if room.ID == "" {
			return NewAPIError(CodeSchema, "room", "id", "room id is required")
		}
		if _, exists := roomByID[room.ID]; exists {
			return NewAPIError(CodeSchema, room.ID, "id", "duplicate room id")
		}
		r := room.Rect
		if r.X1 >= r.X2 || r.Y1 >= r.Y2 {
			return NewAPIError(CodeGeom, room.ID, "rect", "room rectangle is degenerate")
		}
		if r.X1 < s.Bounds.X1 || r.X2 > s.Bounds.X2 || r.Y1 < s.Bounds.Y1 || r.Y2 > s.Bounds.Y2 {
			return NewAPIError(CodeGeom, room.ID, "rect", "room is outside level bounds")
		}
		if apiErr := ValidateRoomParams(room.ID, room.Params); apiErr != nil {
			return apiErr
		}
		roomByID[room.ID] = room
	}

	for i := 0; i < len(s.Rooms); i++ {
		for j := i + 1; j < len(s.Rooms); j++ {
			a := s.Rooms[i]
			b := s.Rooms[j]
			if rectsOverlapArea(a.Rect, b.Rect) {
				return NewAPIError(CodeGeom, a.ID+"/"+b.ID, "rect",
					"rooms %s and %s overlap with positive area", a.ID, b.ID)
			}
		}
	}

	wallByID := map[string]Wall{}
	for _, w := range s.Walls {
		if w.ID == "" {
			return NewAPIError(CodeSchema, "wall", "id", "wall id is required")
		}
		if _, exists := wallByID[w.ID]; exists {
			return NewAPIError(CodeSchema, w.ID, "id", "duplicate wall id")
		}
		axisAligned := (w.X1 == w.X2 && w.Y1 != w.Y2) || (w.Y1 == w.Y2 && w.X1 != w.X2)
		if !axisAligned {
			return NewAPIError(CodeGeom, w.ID, "", "wall must be axis-aligned and non-degenerate")
		}
		for _, roomID := range []string{w.RoomA, w.RoomB} {
			if roomID == "" {
				continue
			}
			room, ok := roomByID[roomID]
			if !ok {
				return NewAPIError(CodeSchema, w.ID, "roomId", "wall references unknown room %q", roomID)
			}
			if !wallOnRoomEdge(w, room.Rect) {
				return NewAPIError(CodeGeom, w.ID, "coordinates",
					"wall %s is not an edge segment of room %s", w.ID, roomID)
			}
		}
		wallByID[w.ID] = w
	}

	openingIDs := map[string]bool{}
	for _, o := range s.Openings {
		if o.ID == "" {
			return NewAPIError(CodeSchema, "opening", "id", "opening id is required")
		}
		if openingIDs[o.ID] {
			return NewAPIError(CodeSchema, o.ID, "id", "duplicate opening id")
		}
		openingIDs[o.ID] = true
		if o.Kind != "door" && o.Kind != "opening" {
			return NewAPIError(CodeSchema, o.ID, "kind", "kind must be door or opening")
		}
		w, ok := wallByID[o.WallID]
		if !ok {
			return NewAPIError(CodeSchema, o.ID, "wallId", "opening references unknown wall %q", o.WallID)
		}
		if !o.Horizontal() && !o.Vertical() {
			return NewAPIError(CodeGeom, o.ID, "coordinates", "opening must be axis-aligned")
		}
		if o.Width() <= 0 {
			return NewAPIError(CodeGeom, o.ID, "width", "opening width must be positive")
		}
		if apiErr := validateOpeningOnWall(o, w); apiErr != nil {
			return apiErr
		}
		if o.RoomA != "" {
			if _, ok := roomByID[o.RoomA]; !ok {
				return NewAPIError(CodeSchema, o.ID, "roomA", "unknown room %q", o.RoomA)
			}
		}
		if o.RoomB != "" {
			if _, ok := roomByID[o.RoomB]; !ok {
				return NewAPIError(CodeSchema, o.ID, "roomB", "unknown room %q", o.RoomB)
			}
		}
		if apiErr := validateOpeningWallPair(o, w); apiErr != nil {
			return apiErr
		}
	}
	for i := 0; i < len(s.Openings); i++ {
		for j := i + 1; j < len(s.Openings); j++ {
			if openingsPositiveOverlap(s.Openings[i], s.Openings[j]) {
				a := s.Openings[i]
				b := s.Openings[j]
				return NewAPIError(CodeGeom, a.ID+"/"+b.ID, "interval",
					"openings %s and %s overlap on wall %s", a.ID, b.ID, a.WallID)
			}
		}
	}

	sourceIDs := map[string]bool{}
	for _, src := range s.Sources {
		if src.ID == "" {
			return NewAPIError(CodeSchema, "source", "id", "source id is required")
		}
		if sourceIDs[src.ID] {
			return NewAPIError(CodeSchema, src.ID, "id", "duplicate source id")
		}
		sourceIDs[src.ID] = true
		p := Point{X: src.X, Y: src.Y}
		if !s.Bounds.Contains(p) {
			return NewAPIError(CodeSchema, src.ID, "coordinates",
				"source %s coordinate (%d,%d) is outside level bounds", src.ID, src.X, src.Y)
		}
		if _, apiErr := RoomAtPoint(s, p, src.ID, "source"); apiErr != nil {
			return apiErr
		}
	}

	listenerIDs := map[string]bool{}
	for _, l := range s.Listeners {
		if l.ID == "" {
			return NewAPIError(CodeSchema, "listener", "id", "listener id is required")
		}
		if listenerIDs[l.ID] {
			return NewAPIError(CodeSchema, l.ID, "id", "duplicate listener id")
		}
		listenerIDs[l.ID] = true
		p := Point{X: l.X, Y: l.Y}
		if !s.Bounds.Contains(p) {
			return NewAPIError(CodeSchema, l.ID, "coordinates",
				"listener %s coordinate (%d,%d) is outside level bounds", l.ID, l.X, l.Y)
		}
		if _, apiErr := RoomAtPoint(s, p, l.ID, "listener"); apiErr != nil {
			return apiErr
		}
	}

	for _, tp := range s.Truth.Paths {
		if !sourceIDs[tp.SourceID] {
			return NewAPIError(CodeSchema, tp.SourceID, "truth.paths.sourceId", "truth references unknown source")
		}
		if !listenerIDs[tp.ListenerID] {
			return NewAPIError(CodeSchema, tp.ListenerID, "truth.paths.listenerId", "truth references unknown listener")
		}
	}
	return nil
}

func openingInterval(o Opening) (int, int) {
	if o.Horizontal() {
		return minMaxInt(o.X1, o.X2)
	}
	return minMaxInt(o.Y1, o.Y2)
}

func openingsPositiveOverlap(a, b Opening) bool {
	if a.WallID != b.WallID {
		return false
	}
	alo, ahi := openingInterval(a)
	blo, bhi := openingInterval(b)
	return maxInt(alo, blo) < minInt(ahi, bhi)
}

func rectsOverlapArea(a, b Rect) bool {
	return maxInt(a.X1, b.X1) < minInt(a.X2, b.X2) &&
		maxInt(a.Y1, b.Y1) < minInt(a.Y2, b.Y2)
}

func wallOnRoomEdge(w Wall, r Rect) bool {
	if w.Horizontal() {
		if w.Y1 != r.Y1 && w.Y1 != r.Y2 {
			return false
		}
		lo, hi := minMaxInt(w.X1, w.X2)
		return lo >= r.X1 && hi <= r.X2
	}
	if w.X1 != r.X1 && w.X1 != r.X2 {
		return false
	}
	lo, hi := minMaxInt(w.Y1, w.Y2)
	return lo >= r.Y1 && hi <= r.Y2
}

func validateOpeningOnWall(o Opening, w Wall) *APIError {
	if o.Horizontal() != w.Horizontal() || o.Vertical() != w.Vertical() {
		return NewAPIError(CodeGeom, o.ID, "wallId", "opening %s orientation does not match wall %s", o.ID, w.ID)
	}
	if w.Horizontal() {
		if o.Y1 != w.Y1 {
			return NewAPIError(CodeGeom, o.ID, "y1", "opening %s is not on wall %s", o.ID, w.ID)
		}
		olo, ohi := minMaxInt(o.X1, o.X2)
		wlo, whi := minMaxInt(w.X1, w.X2)
		if olo < wlo || ohi > whi {
			return NewAPIError(CodeGeom, o.ID, "wallId",
				"opening %s interval is outside wall %s", o.ID, w.ID)
		}
		return nil
	}
	if o.X1 != w.X1 {
		return NewAPIError(CodeGeom, o.ID, "x1", "opening %s is not on wall %s", o.ID, w.ID)
	}
	olo, ohi := minMaxInt(o.Y1, o.Y2)
	wlo, whi := minMaxInt(w.Y1, w.Y2)
	if olo < wlo || ohi > whi {
		return NewAPIError(CodeGeom, o.ID, "wallId",
			"opening %s interval is outside wall %s", o.ID, w.ID)
	}
	return nil
}

func validateOpeningWallPair(o Opening, w Wall) *APIError {
	if o.RoomA == "" && o.RoomB == "" {
		return nil
	}
	openingPair := pairKey(o.RoomA, o.RoomB)
	wallPair := pairKey(w.RoomA, w.RoomB)
	if openingPair == "?" {
		return nil
	}
	if openingPair != wallPair {
		return NewAPIError(CodeGeom, o.ID, "wallId",
			"opening %s points to wall %s which is not the adjacent segment for rooms %s/%s",
			o.ID, o.WallID, o.RoomA, o.RoomB)
	}
	return nil
}

func pairKey(a, b string) string {
	if a == "" || b == "" {
		if a == "" {
			return b
		}
		return a
	}
	if a > b {
		a, b = b, a
	}
	return a + "|" + b
}

func RoomAtPoint(s *Sample, p Point, objectID, objectKind string) (string, *APIError) {
	var found string
	for _, room := range s.Rooms {
		if room.Rect.ContainsInterior(p) {
			if found != "" {
				return "", NewAPIError(CodeGeom, objectID, "coordinates",
					"%s %s is ambiguously inside rooms %s and %s", objectKind, objectID, found, room.ID)
			}
			found = room.ID
		}
	}
	if found == "" {
		return "", NewAPIError(CodeGeom, objectID, "coordinates",
			"%s %s at (%d,%d) is not inside any room", objectKind, objectID, p.X, p.Y)
	}
	return found, nil
}

func ValidatePrecision(p PrecisionSpec) *APIError {
	if p.Scale != FixedScale {
		return NewAPIError(CodePara, "precision", "precision.scale",
			"scale must be declared fixed-point scale %d", FixedScale)
	}
	if p.SampleCount < 2 || p.SampleCount > 512 {
		return NewAPIError(CodePara, "precision", "precision.sampleCount",
			"sampleCount must be between 2 and 512")
	}
	if p.DeltaTMS < 1 || p.DeltaTMS > 1000 {
		return NewAPIError(CodePara, "precision", "precision.dtMs",
			"dtMs must be between 1 and 1000")
	}
	if p.ToleranceDB < 0 || p.ToleranceDB > 100000 {
		return NewAPIError(CodePara, "precision", "precision.toleranceDb",
			"toleranceDb is out of range")
	}
	if p.MaxParallel < 1 || p.MaxParallel > 8 {
		return NewAPIError(CodePara, "precision", "precision.maxParallel",
			"maxParallel must be between 1 and 8")
	}
	if p.BufferSampleCap < 1 || p.BufferSampleCap > 4096 {
		return NewAPIError(CodePara, "precision", "precision.bufferSampleCap",
			"bufferSampleCap must be between 1 and 4096")
	}
	if p.PathAttenPerUnit < 0 || p.PathAttenPerUnit > 10000 {
		return NewAPIError(CodePara, "precision", "precision.pathAttenPerUnitDb",
			"pathAttenPerUnitDb is out of range")
	}
	if p.OcclusionAttenDB < 0 || p.OcclusionAttenDB > 120000 {
		return NewAPIError(CodePara, "precision", "precision.occlusionAttenDb",
			"occlusionAttenDb is out of range")
	}
	return nil
}

func ValidateRoomParams(roomID string, p RoomParams) *APIError {
	if p.RT60MS < 100 || p.RT60MS > 10000 {
		return NewAPIError(CodePara, roomID, "rooms."+roomID+".params.rt60Ms",
			"rt60Ms must be between 100 and 10000")
	}
	if p.EarlyReflectionDensity < 0 || int64(p.EarlyReflectionDensity) > FixedScale {
		return NewAPIError(CodePara, roomID, "rooms."+roomID+".params.earlyReflectionDensity",
			"earlyReflectionDensity must be in [0,1]")
	}
	if p.GainDB < -24000 || p.GainDB > 24000 {
		return NewAPIError(CodePara, roomID, "rooms."+roomID+".params.gainDb",
			"gainDb must be in [-24,24] dB")
	}
	return nil
}
