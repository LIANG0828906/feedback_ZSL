package acousticfield

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
)

// LoadLevelJSON decodes a level document and verifies required field presence
// before applying semantic validation.
func LoadLevelJSON(data []byte) (*Level, error) {
	var top map[string]json.RawMessage
	dec := json.NewDecoder(bytes.NewReader(data))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&top); err != nil {
		return nil, schemaError("level", "$", "invalid JSON: "+err.Error())
	}
	if err := checkJSONFields(top, "level", "$", "id", "seed", "bounds", "bands", "chunks", "sources", "listeners"); err != nil {
		return nil, err
	}
	if err := checkNestedJSON(top); err != nil {
		return nil, err
	}
	var level Level
	if err := json.Unmarshal(data, &level); err != nil {
		return nil, schemaError("level", "$", "invalid level document: "+err.Error())
	}
	if err := ValidateLevel(&level); err != nil {
		return nil, err
	}
	return &level, nil
}

func DecodeLevel(r io.Reader) (*Level, error) {
	data, err := io.ReadAll(r)
	if err != nil {
		return nil, schemaError("level", "$", "read failed: "+err.Error())
	}
	return LoadLevelJSON(data)
}

func checkJSONFields(obj map[string]json.RawMessage, objectID, prefix string, fields ...string) error {
	for _, f := range fields {
		if _, ok := obj[f]; !ok {
			return schemaError(objectID, prefix+"."+f, "required field is missing")
		}
	}
	return nil
}

func checkNestedJSON(top map[string]json.RawMessage) error {
	if err := checkBoundsJSON(top["bounds"], "level", "$.bounds"); err != nil {
		return err
	}
	var bands []map[string]json.RawMessage
	if err := json.Unmarshal(top["bands"], &bands); err != nil {
		return schemaError("level", "$.bands", "expected an array of objects")
	}
	for i, item := range bands {
		obj := objectID(item, "band", i)
		if err := checkJSONFields(item, obj, fmt.Sprintf("$.bands[%d]", i), "id", "center_hz"); err != nil {
			return err
		}
	}

	var chunks []map[string]json.RawMessage
	if err := json.Unmarshal(top["chunks"], &chunks); err != nil {
		return schemaError("level", "$.chunks", "expected an array of objects")
	}
	for i, item := range chunks {
		obj := objectID(item, "chunk", i)
		prefix := fmt.Sprintf("$.chunks[%d]", i)
		if err := checkJSONFields(item, obj, prefix, "id", "bounds", "absorption", "reflectors"); err != nil {
			return err
		}
		if err := checkBoundsJSON(item["bounds"], obj, prefix+".bounds"); err != nil {
			return err
		}
		var absorption []map[string]json.RawMessage
		if err := json.Unmarshal(item["absorption"], &absorption); err != nil {
			return schemaError(obj, prefix+".absorption", "expected an array of objects")
		}
		for j, a := range absorption {
			if err := checkJSONFields(a, obj, fmt.Sprintf("%s.absorption[%d]", prefix, j), "band_id", "coefficient_q16"); err != nil {
				return err
			}
		}
		var reflectors []map[string]json.RawMessage
		if err := json.Unmarshal(item["reflectors"], &reflectors); err != nil {
			return schemaError(obj, prefix+".reflectors", "expected an array of objects")
		}
		for j, refl := range reflectors {
			rp := fmt.Sprintf("%s.reflectors[%d]", prefix, j)
			if err := checkJSONFields(refl, obj, rp, "id", "a", "b", "normal"); err != nil {
				return err
			}
			if err := checkPointJSON(refl["a"], obj, rp+".a"); err != nil {
				return err
			}
			if err := checkPointJSON(refl["b"], obj, rp+".b"); err != nil {
				return err
			}
			if err := checkVectorJSON(refl["normal"], obj, rp+".normal"); err != nil {
				return err
			}
		}
	}

	var sources []map[string]json.RawMessage
	if err := json.Unmarshal(top["sources"], &sources); err != nil {
		return schemaError("level", "$.sources", "expected an array of objects")
	}
	for i, item := range sources {
		obj := objectID(item, "source", i)
		prefix := fmt.Sprintf("$.sources[%d]", i)
		if err := checkJSONFields(item, obj, prefix, "id", "chunk_id", "position", "emission"); err != nil {
			return err
		}
		if err := checkPointJSON(item["position"], obj, prefix+".position"); err != nil {
			return err
		}
		var emission []map[string]json.RawMessage
		if err := json.Unmarshal(item["emission"], &emission); err != nil {
			return schemaError(obj, prefix+".emission", "expected an array of objects")
		}
		for j, e := range emission {
			if err := checkJSONFields(e, obj, fmt.Sprintf("%s.emission[%d]", prefix, j), "band_id", "energy"); err != nil {
				return err
			}
		}
	}

	var listeners []map[string]json.RawMessage
	if err := json.Unmarshal(top["listeners"], &listeners); err != nil {
		return schemaError("level", "$.listeners", "expected an array of objects")
	}
	for i, item := range listeners {
		obj := objectID(item, "listener", i)
		prefix := fmt.Sprintf("$.listeners[%d]", i)
		if err := checkJSONFields(item, obj, prefix, "id", "chunk_id", "point"); err != nil {
			return err
		}
		if err := checkPointJSON(item["point"], obj, prefix+".point"); err != nil {
			return err
		}
	}
	return nil
}

func objectID(obj map[string]json.RawMessage, kind string, index int) string {
	if raw, ok := obj["id"]; ok {
		var id any
		if json.Unmarshal(raw, &id) == nil {
			return fmt.Sprint(id)
		}
	}
	return fmt.Sprintf("%s-%d", kind, index)
}

func checkBoundsJSON(raw json.RawMessage, obj, path string) error {
	var bounds map[string]json.RawMessage
	if err := json.Unmarshal(raw, &bounds); err != nil {
		return schemaError(obj, path, "expected a bounds object")
	}
	return checkJSONFields(bounds, obj, path, "min_x", "min_y", "max_x", "max_y")
}

func checkPointJSON(raw json.RawMessage, obj, path string) error {
	var point map[string]json.RawMessage
	if err := json.Unmarshal(raw, &point); err != nil {
		return schemaError(obj, path, "expected a point object")
	}
	return checkJSONFields(point, obj, path, "x", "y")
}

func checkVectorJSON(raw json.RawMessage, obj, path string) error {
	var vec map[string]json.RawMessage
	if err := json.Unmarshal(raw, &vec); err != nil {
		return schemaError(obj, path, "expected a vector object")
	}
	return checkJSONFields(vec, obj, path, "x", "y")
}

// ValidateLevel validates schema and numeric domains. It returns the first
// failure with a stable object identifier and field path.
func ValidateLevel(level *Level) error {
	if level == nil {
		return schemaError("level", "$", "level is nil")
	}
	if level.ID == "" {
		return schemaError("level", "$.id", "identifier is required")
	}
	if err := validateRect(level.Bounds, "level", "$.bounds"); err != nil {
		return err
	}
	if len(level.Bands) == 0 {
		return schemaError("level", "$.bands", "at least one band is required")
	}
	if len(level.Chunks) == 0 {
		return schemaError("level", "$.chunks", "at least one chunk is required")
	}

	bands := make(map[string]Band, len(level.Bands))
	for i, band := range level.Bands {
		path := fmt.Sprintf("$.bands[%d]", i)
		if band.ID == "" {
			return schemaError("level", path+".id", "band identifier is missing")
		}
		if _, dup := bands[band.ID]; dup {
			return &FieldError{Code: CodeValue, ObjectID: level.ID, FieldPath: path + ".id", Measured: band.ID, Message: "duplicate band id"}
		}
		if band.CenterHz <= 0 {
			return valueError(band.ID, path+".center_hz", band.CenterHz, 1, int64(1_000_000_000))
		}
		if band.CenterHz > 1_000_000_000 {
			return valueError(band.ID, path+".center_hz", band.CenterHz, 1, int64(1_000_000_000))
		}
		bands[band.ID] = band
	}

	chunkIDs := make(map[int]Chunk, len(level.Chunks))
	var area sumArea
	for i := range level.Chunks {
		c := &level.Chunks[i]
		obj := fmt.Sprintf("chunk-%d", c.ID)
		if c.ID < 0 {
			return valueError(obj, fmt.Sprintf("$.chunks[%d].id", i), c.ID, 0, int64(^uint64(0)>>1))
		}
		if _, dup := chunkIDs[c.ID]; dup {
			return &FieldError{Code: CodeValue, ObjectID: obj, FieldPath: fmt.Sprintf("$.chunks[%d].id", i), Measured: c.ID, Message: "duplicate chunk id"}
		}
		if err := validateRect(c.Bounds, obj, fmt.Sprintf("$.chunks[%d].bounds", i)); err != nil {
			return err
		}
		if !rectInside(c.Bounds, level.Bounds) {
			return valueError(obj, fmt.Sprintf("$.chunks[%d].bounds", i), fmt.Sprintf("[%d,%d]-[%d,%d]", c.Bounds.MinX, c.Bounds.MinY, c.Bounds.MaxX, c.Bounds.MaxY), 0, "within level bounds")
		}
		if _, err := area.add(c.Bounds); err != nil {
			return err
		}

		abs := make(map[string]int64, len(c.Absorption))
		for j, a := range c.Absorption {
			p := fmt.Sprintf("$.chunks[%d].absorption[%d]", i, j)
			if a.BandID == "" {
				return schemaError(obj, p+".band_id", "band reference is missing")
			}
			if _, ok := bands[a.BandID]; !ok {
				return &FieldError{Code: CodeValue, ObjectID: obj, FieldPath: p + ".band_id", Measured: a.BandID, Message: "unknown band"}
			}
			if _, dup := abs[a.BandID]; dup {
				return &FieldError{Code: CodeValue, ObjectID: obj, FieldPath: p + ".band_id", Measured: a.BandID, Message: "duplicate absorption for band"}
			}
			if a.Coefficient <= 0 || a.Coefficient > Q16 {
				return valueError(obj, p+".coefficient_q16", a.Coefficient, 1, Q16)
			}
			abs[a.BandID] = a.Coefficient
		}
		for bandID := range bands {
			if _, ok := abs[bandID]; !ok {
				return schemaError(obj, fmt.Sprintf("$.chunks[%d].absorption", i), "absorption coefficient missing for band "+bandID)
			}
		}

		reflectorIDs := make(map[string]struct{}, len(c.Reflectors))
		for j, refl := range c.Reflectors {
			p := fmt.Sprintf("$.chunks[%d].reflectors[%d]", i, j)
			if refl.ID == "" {
				return schemaError(obj, p+".id", "reflector identifier is missing")
			}
			if _, dup := reflectorIDs[refl.ID]; dup {
				return &FieldError{Code: CodeValue, ObjectID: obj, FieldPath: p + ".id", Measured: refl.ID, Message: "duplicate reflector id"}
			}
			reflectorIDs[refl.ID] = struct{}{}
			if refl.A == refl.B {
				return valueError(obj, p+".b", fmt.Sprintf("%+v", refl.B), "distinct endpoints", "distinct endpoints")
			}
			if !pointInRect(refl.A, c.Bounds) || !pointInRect(refl.B, c.Bounds) {
				return valueError(obj, p, "outside chunk bounds", "inside chunk", "inside chunk")
			}
			lenSq := refl.Normal.X*refl.Normal.X + refl.Normal.Y*refl.Normal.Y
			want := Q16 * Q16
			lenTolerance := want/1000 + 2*Q16
			if lenSq < want-lenTolerance || lenSq > want+lenTolerance {
				return valueError(obj, p+".normal", fmt.Sprintf("len2=%d", lenSq), want-lenTolerance, want+lenTolerance)
			}
			dx := refl.B.X - refl.A.X
			dy := refl.B.Y - refl.A.Y
			dot := dx*refl.Normal.X + dy*refl.Normal.Y
			if dot < 0 {
				dot = -dot
			}
			perpTolerance := Distance(refl.A, refl.B)*Q16/100 + Q16
			if dot > perpTolerance {
				return valueError(obj, p+".normal", fmt.Sprintf("dot=%d", dot), 0, perpTolerance)
			}
		}
		chunkIDs[c.ID] = *c
	}
	if area.overflows {
		return &FieldError{Code: CodeValue, ObjectID: level.ID, FieldPath: "$.chunks", Message: "chunk area overflow"}
	}
	if area.total != rectArea(level.Bounds) {
		return valueError(level.ID, "$.chunks", fmt.Sprintf("area=%d", area.total), rectArea(level.Bounds), rectArea(level.Bounds))
	}
	chunks := make([]Chunk, 0, len(level.Chunks))
	for _, c := range level.Chunks {
		chunks = append(chunks, c)
	}
	for i := 0; i < len(chunks); i++ {
		for j := i + 1; j < len(chunks); j++ {
			if rectOverlap(chunks[i].Bounds, chunks[j].Bounds) {
				return valueError(fmt.Sprintf("chunk-%d", chunks[i].ID), "$.chunks", fmt.Sprintf("overlaps chunk-%d", chunks[j].ID), "no overlap", "no overlap")
			}
		}
	}

	sourceIDs := make(map[string]struct{}, len(level.Sources))
	for i, src := range level.Sources {
		obj := src.ID
		if obj == "" {
			obj = fmt.Sprintf("source-%d", i)
		}
		p := fmt.Sprintf("$.sources[%d]", i)
		if src.ID == "" {
			return schemaError(obj, p+".id", "source identifier is missing")
		}
		if _, dup := sourceIDs[src.ID]; dup {
			return &FieldError{Code: CodeValue, ObjectID: obj, FieldPath: p + ".id", Measured: src.ID, Message: "duplicate source id"}
		}
		sourceIDs[src.ID] = struct{}{}
		c, ok := chunkIDs[src.ChunkID]
		if !ok {
			return &FieldError{Code: CodeValue, ObjectID: obj, FieldPath: p + ".chunk_id", Measured: src.ChunkID, Message: "unknown chunk"}
		}
		if !pointInRect(src.Position, level.Bounds) {
			return valueError(obj, p+".position", fmt.Sprintf("%+v", src.Position), "inside level", "inside level")
		}
		if !pointInRect(src.Position, c.Bounds) {
			return valueError(obj, p+".position", fmt.Sprintf("%+v", src.Position), fmt.Sprintf("inside chunk-%d", src.ChunkID), fmt.Sprintf("inside chunk-%d", src.ChunkID))
		}
		em := make(map[string]int64, len(src.Emission))
		for j, e := range src.Emission {
			ep := fmt.Sprintf("%s.emission[%d]", p, j)
			if e.BandID == "" {
				return schemaError(obj, ep+".band_id", "band reference is missing")
			}
			if _, ok := bands[e.BandID]; !ok {
				return &FieldError{Code: CodeValue, ObjectID: obj, FieldPath: ep + ".band_id", Measured: e.BandID, Message: "unknown band"}
			}
			if _, dup := em[e.BandID]; dup {
				return &FieldError{Code: CodeValue, ObjectID: obj, FieldPath: ep + ".band_id", Measured: e.BandID, Message: "duplicate emission for band"}
			}
			if e.Energy <= 0 {
				return valueError(obj, ep+".energy", e.Energy, 1, maxEnergyUnit)
			}
			if e.Energy > maxEnergyUnit {
				return valueError(obj, ep+".energy", e.Energy, 1, maxEnergyUnit)
			}
			em[e.BandID] = e.Energy
		}
		for bandID := range bands {
			if _, ok := em[bandID]; !ok {
				return schemaError(obj, fmt.Sprintf("%s.emission", p), "emission missing for band "+bandID)
			}
		}
	}

	listenerIDs := make(map[string]struct{}, len(level.Listeners))
	for i, lis := range level.Listeners {
		obj := lis.ID
		if obj == "" {
			obj = fmt.Sprintf("listener-%d", i)
		}
		p := fmt.Sprintf("$.listeners[%d]", i)
		if lis.ID == "" {
			return schemaError(obj, p+".id", "listener identifier is missing")
		}
		if _, dup := listenerIDs[lis.ID]; dup {
			return &FieldError{Code: CodeValue, ObjectID: obj, FieldPath: p + ".id", Measured: lis.ID, Message: "duplicate listener id"}
		}
		listenerIDs[lis.ID] = struct{}{}
		c, ok := chunkIDs[lis.ChunkID]
		if !ok {
			return &FieldError{Code: CodeValue, ObjectID: obj, FieldPath: p + ".chunk_id", Measured: lis.ChunkID, Message: "unknown chunk"}
		}
		if !pointInRect(lis.Point, level.Bounds) {
			return valueError(obj, p+".point", fmt.Sprintf("%+v", lis.Point), "inside level", "inside level")
		}
		if !pointInRect(lis.Point, c.Bounds) {
			return valueError(obj, p+".point", fmt.Sprintf("%+v", lis.Point), fmt.Sprintf("inside chunk-%d", lis.ChunkID), fmt.Sprintf("inside chunk-%d", lis.ChunkID))
		}
	}
	return nil
}

type sumArea struct {
	total     int64
	overflows bool
}

func (s *sumArea) add(r Rect) (int64, error) {
	a := rectArea(r)
	if a < 0 || s.total > (1<<63-1)-a {
		s.overflows = true
		return 0, valueError("level", "$.chunks", "area overflow", 0, int64(1<<63-1))
	}
	s.total += a
	return s.total, nil
}

func validateRect(r Rect, objectID, path string) error {
	if r.MinX < -maxCoordinate || r.MaxX > maxCoordinate {
		return valueError(objectID, path+".x", fmt.Sprintf("[%d,%d]", r.MinX, r.MaxX), -maxCoordinate, maxCoordinate)
	}
	if r.MinY < -maxCoordinate || r.MaxY > maxCoordinate {
		return valueError(objectID, path+".y", fmt.Sprintf("[%d,%d]", r.MinY, r.MaxY), -maxCoordinate, maxCoordinate)
	}
	if r.MaxX <= r.MinX {
		return valueError(objectID, path+".max_x", r.MaxX, fmt.Sprintf("> %d", r.MinX), int64(^uint64(0)>>1))
	}
	if r.MaxY <= r.MinY {
		return valueError(objectID, path+".max_y", r.MaxY, fmt.Sprintf("> %d", r.MinY), int64(^uint64(0)>>1))
	}
	return nil
}
