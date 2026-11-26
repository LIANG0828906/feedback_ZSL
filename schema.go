package maskqc

import (
	"bytes"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"sort"
)

const (
	SchemaV1 = "mask-qc.project/v1"
	SchemaV2 = "mask-qc.project/v2"
	SchemaV3 = "mask-qc.project/v3"

	DefaultHardnessPct = 50
	DefaultPriority    = 100
	DefaultTileSize    = 16
)

type ExportOptions struct {
	IncludeAssets bool
	IncludeAudit  bool
}

type ExportResult struct {
	SchemaVersion string
	Data          []byte
	Trace         []TraceEntry
	Lossy         []LossyItem
}

func schemaError(path, objectID, format string, args ...any) *BizError {
	return &BizError{
		Code: CodeSchema, Message: fmt.Sprintf(format, args...),
		Path: path, ObjectID: objectID, ChunkID: -1,
	}
}

func decodeJSON(data []byte) (map[string]any, error) {
	dec := json.NewDecoder(bytes.NewReader(data))
	dec.UseNumber()
	var root map[string]any
	if err := dec.Decode(&root); err != nil {
		return nil, schemaError("$", "", "invalid JSON: %v", err)
	}
	if root == nil {
		return nil, schemaError("$", "", "root must be an object")
	}
	return root, nil
}

func asMap(v any) (map[string]any, bool) {
	m, ok := v.(map[string]any)
	return m, ok
}

func asArray(v any) ([]any, bool) {
	a, ok := v.([]any)
	return a, ok
}

func asString(v any) (string, bool) {
	s, ok := v.(string)
	return s, ok && s != ""
}

func asInt(v any) (int64, bool) {
	switch n := v.(type) {
	case json.Number:
		i, err := n.Int64()
		return i, err == nil
	case int64:
		return n, true
	case int:
		return int64(n), true
	case float64:
		return int64(n), true
	default:
		return 0, false
	}
}

func requiredString(m map[string]any, key, path, objectID string) (string, error) {
	v, ok := m[key]
	if !ok || v == nil {
		err := schemaError(path+"."+key, objectID, "missing required string field")
		if err.ObjectID == "" {
			err.ObjectID = path
		}
		return "", err
	}
	s, ok := asString(v)
	if !ok {
		err := schemaError(path+"."+key, objectID, "field must be a non-empty string")
		if err.ObjectID == "" {
			err.ObjectID = path
		}
		return "", err
	}
	return s, nil
}

func requiredInt(m map[string]any, key, path, objectID string) (int64, error) {
	v, ok := m[key]
	if !ok || v == nil {
		err := schemaError(path+"."+key, objectID, "missing required integer field")
		if err.ObjectID == "" {
			err.ObjectID = path
		}
		return 0, err
	}
	n, ok := asInt(v)
	if !ok {
		err := schemaError(path+"."+key, objectID, "field must be an integer")
		if err.ObjectID == "" {
			err.ObjectID = path
		}
		return 0, err
	}
	return n, nil
}

func optionalIntDefault(m map[string]any, key string, def int64) (int64, bool) {
	v, ok := m[key]
	if !ok || v == nil {
		return def, false
	}
	n, ok := asInt(v)
	if !ok {
		return def, false
	}
	return n, true
}

func objectIDFromRoot(root map[string]any) string {
	for _, key := range []string{"project_id", "projectId", "id", "name"} {
		if s, ok := asString(root[key]); ok {
			return s
		}
	}
	if meta, ok := asMap(root["meta"]); ok {
		for _, key := range []string{"project_id", "projectId", "id", "name"} {
			if s, ok := asString(meta[key]); ok {
				return s
			}
		}
	}
	return ""
}

// LoadProject detects one of the three supported schema versions and maps it
// to the internal model. All numeric JSON values are decoded as json.Number.
func LoadProject(data []byte) (*Project, error) {
	root, err := decodeJSON(data)
	if err != nil {
		return nil, err
	}
	objectID := objectIDFromRoot(root)
	version, _ := root["schema"].(string)
	if version == "" {
		version, _ = root["schema_version"].(string)
	}
	if version == "" {
		if v, ok := root["version"]; ok {
			if s, ok := asString(v); ok {
				version = s
			} else if n, ok := asInt(v); ok {
				version = fmt.Sprintf("%d", n)
			}
		}
	}
	switch version {
	case SchemaV1, "v1", "1", "1.0":
		return loadV1(root, objectID)
	case SchemaV2, "v2", "2", "2.0":
		return loadV2(root, objectID)
	case SchemaV3, "v3", "3", "3.0":
		return loadV3(root, objectID)
	case "":
		return nil, schemaError("schema", objectID, "missing schema version")
	default:
		return nil, schemaError("schema", objectID, "unknown schema version %q", version)
	}
}

func canvasSize(root map[string]any, objectID string, keys ...string) (int, int, int, error) {
	var width, height int64
	var tile int64 = DefaultTileSize
	found := false
	for _, key := range keys {
		if m, ok := asMap(root[key]); ok {
			found = true
			w, err := requiredInt(m, "width", key, objectID)
			if err != nil {
				w2, err2 := requiredInt(m, "w", key, objectID)
				if err2 != nil {
					return 0, 0, 0, err
				}
				w = w2
			}
			h, err := requiredInt(m, "height", key, objectID)
			if err != nil {
				h2, err2 := requiredInt(m, "h", key, objectID)
				if err2 != nil {
					return 0, 0, 0, err
				}
				h = h2
			}
			width, height = w, h
			if t, ok := optionalIntDefault(m, "tile", DefaultTileSize); ok {
				tile = t
			} else if t, ok := optionalIntDefault(m, "tile_width", DefaultTileSize); ok {
				tile = t
			}
			break
		}
	}
	if !found {
		w, err1 := requiredInt(root, "width", "$", objectID)
		h, err2 := requiredInt(root, "height", "$", objectID)
		if err1 != nil {
			return 0, 0, 0, err1
		}
		if err2 != nil {
			return 0, 0, 0, err2
		}
		width, height = w, h
		if t, ok := optionalIntDefault(root, "tile_width", DefaultTileSize); ok {
			tile = t
		}
	}
	if width <= 0 || height <= 0 {
		return 0, 0, 0, schemaError("canvas", objectID, "width and height must be positive")
	}
	if tile <= 0 {
		tile = DefaultTileSize
	}
	return int(width), int(height), int(tile), nil
}

func parsePointArray(v any, path, objectID string) (Point, Point, error) {
	a, ok := asArray(v)
	if !ok || len(a) != 4 {
		return Point{}, Point{}, schemaError(path, objectID, "must be [x1,y1,x2,y2]")
	}
	vals := make([]int, 4)
	for i, item := range a {
		n, ok := asInt(item)
		if !ok {
			return Point{}, Point{}, schemaError(fmt.Sprintf("%s[%d]", path, i), objectID, "coordinate must be integer")
		}
		vals[i] = int(n)
	}
	return Point{X: vals[0], Y: vals[1]}, Point{X: vals[2], Y: vals[3]}, nil
}

func parsePointObject(m map[string]any, key, path, objectID string) (Point, error) {
	pm, ok := asMap(m[key])
	if !ok {
		return Point{}, schemaError(path+"."+key, objectID, "missing point object")
	}
	x, err := requiredInt(pm, "x", path+"."+key, objectID)
	if err != nil {
		return Point{}, err
	}
	y, err := requiredInt(pm, "y", path+"."+key, objectID)
	if err != nil {
		return Point{}, err
	}
	return Point{X: int(x), Y: int(y)}, nil
}

func dirFromV1(code int64, has bool, objectID, path string) (DirClass, TraceEntry) {
	if !has {
		return DirMixed, TraceEntry{
			Path: path, Field: "direction_code", Rule: "default-when-missing",
			OldValue: "<missing>", NewValue: string(DirMixed), SourceDefault: string(DirMixed),
		}
	}
	switch code {
	case 0:
		return DirTangent, TraceEntry{Rule: "v1-direction-code", OldValue: "0", NewValue: string(DirTangent)}
	case 1:
		return DirNormalAlign, TraceEntry{Rule: "v1-direction-code", OldValue: "1", NewValue: string(DirNormalAlign)}
	case 2:
		return DirNormalOppose, TraceEntry{Rule: "v1-direction-code", OldValue: "2", NewValue: string(DirNormalOppose)}
	case 3:
		return DirDiagonal, TraceEntry{Rule: "v1-direction-code", OldValue: "3", NewValue: string(DirDiagonal)}
	default:
		return DirMixed, TraceEntry{
			Rule: "unknown-v1-direction-code-default", OldValue: fmt.Sprint(code),
			NewValue: string(DirMixed), SourceDefault: string(DirMixed),
		}
	}
}

func dirFromV2(code string) DirClass {
	switch code {
	case "T":
		return DirTangent
	case "A":
		return DirNormalAlign
	case "O":
		return DirNormalOppose
	case "D":
		return DirDiagonal
	default:
		return DirMixed
	}
}

func v1CodeForDir(d DirClass) (int, bool) {
	switch d {
	case DirTangent:
		return 0, true
	case DirNormalAlign:
		return 1, true
	case DirNormalOppose:
		return 2, true
	case DirDiagonal:
		return 3, true
	default:
		return -1, false
	}
}

func v2CodeForDir(d DirClass) string {
	switch d {
	case DirTangent:
		return "T"
	case DirNormalAlign:
		return "A"
	case DirNormalOppose:
		return "O"
	case DirDiagonal:
		return "D"
	default:
		return "X"
	}
}

func applySidecar(raw map[string]any, p *Project) {
	x, ok := asMap(raw["x_maskqc"])
	if !ok {
		return
	}
	if originals, ok := asMap(x["weight_myriad"]); ok {
		for i := range p.Strokes {
			if v, ok := originals[p.Strokes[i].ID]; ok {
				if n, ok := asInt(v); ok {
					p.Strokes[i].WeightMyriad = int(n)
					p.AppendTrace("load-sidecar", "x_maskqc.weight_myriad."+p.Strokes[i].ID, TraceEntry{
						ObjectID: p.Strokes[i].ID, Field: "weight_myriad",
						Rule: "restore-exact-original", NewValue: fmt.Sprint(n),
					})
				}
			}
		}
	}
	if rem, ok := asMap(x["weight_remainders"]); ok {
		for i := range p.Strokes {
			if v, ok := rem[p.Strokes[i].ID]; ok {
				if n, ok := asInt(v); ok {
					// At load time v1 weight was already mapped permille*10.
					permille := p.Strokes[i].WeightMyriad / 10
					restored, err := RestoreMyriad(permille, int(n))
					if err == nil {
						p.Strokes[i].WeightMyriad = restored
						p.AppendTrace("load-sidecar", "x_maskqc.weight_remainders."+p.Strokes[i].ID, TraceEntry{
							ObjectID: p.Strokes[i].ID, Field: "weight_myriad",
							Rule: "restore-remainder", NewValue: fmt.Sprint(restored), Remainder: n,
						})
					}
				}
			}
		}
	}
	if sc, ok := asMap(x["sidecar"]); ok {
		if groups, ok := sc["groups"].([]any); ok {
			data, _ := json.Marshal(groups)
			var gs []StrokeGroup
			if json.Unmarshal(data, &gs) == nil {
				p.Groups = gs
				p.AppendTrace("load-sidecar", "x_maskqc.sidecar.groups", TraceEntry{Rule: "restore-lossy-groups", NewValue: fmt.Sprintf("%d groups", len(gs))})
			}
		}
		if dirs, ok := asMap(sc["directions"]); ok {
			for i := range p.Strokes {
				if v, ok := dirs[p.Strokes[i].ID].(string); ok {
					p.Strokes[i].DirectionClass = DirClass(v)
				}
			}
		}
	}
	if lossy, ok := x["lossy_report"].([]any); ok {
		data, _ := json.Marshal(lossy)
		var items []LossyItem
		if json.Unmarshal(data, &items) == nil {
			p.Lossy = items
		}
	}
}

func loadV1(root map[string]any, objectID string) (*Project, error) {
	id, err := requiredString(root, "project_id", "$", objectID)
	if err != nil {
		// Also accept id.
		if id2, err2 := requiredString(root, "id", "$", objectID); err2 == nil {
			id = id2
		} else {
			return nil, err
		}
	}
	objectID = id
	w, h, tile, err := canvasSize(root, objectID, "canvas")
	if err != nil {
		return nil, err
	}
	cons, ok := asMap(root["construction"])
	if !ok {
		return nil, schemaError("construction", objectID, "missing construction object")
	}
	lines, ok := asArray(cons["lines"])
	if !ok {
		return nil, schemaError("construction.lines", objectID, "missing lines array")
	}
	p := &Project{
		ID: id, SourceVersion: "v1", SchemaVersion: SchemaV1,
		Width: w, Height: h, TileWidth: tile, TileHeight: tile,
	}
	if seed, ok := optionalIntDefault(root, "seed", 0); ok {
		p.Seed = seed
	}
	for i, raw := range lines {
		obj, ok := asMap(raw)
		if !ok {
			return nil, schemaError(fmt.Sprintf("construction.lines[%d]", i), "", "stroke must be object")
		}
		base := fmt.Sprintf("construction.lines[%d]", i)
		sid, err := requiredString(obj, "id", base, "")
		if err != nil {
			return nil, err
		}
		path := base + "{id=" + sid + "}"
		s := Stroke{ID: sid}
		if seq, ok := optionalIntDefault(obj, "line_no", int64(i+1)); ok {
			s.Seq = int(seq)
		}
		if pr, ok := optionalIntDefault(obj, "process_priority", DefaultPriority); ok {
			s.Priority = int(pr)
		} else {
			s.Priority = DefaultPriority
			p.AppendTrace("load-v1", path+".process_priority", TraceEntry{
				ObjectID: sid, Field: "priority", OldValue: "<missing>",
				NewValue: fmt.Sprint(DefaultPriority), SourceDefault: fmt.Sprint(DefaultPriority),
				Rule: "default-when-missing",
			})
		}
		st, en, err := parsePointArray(obj["points"], path+".points", sid)
		if err != nil {
			return nil, err
		}
		s.Start, s.End = st, en
		width, err := requiredInt(obj, "brush_width", path, sid)
		if err != nil {
			return nil, err
		}
		s.Width = int(width)
		wp, err := requiredInt(obj, "weight_permille", path, sid)
		if err != nil {
			return nil, err
		}
		if wp < 0 || wp > 1000 {
			return nil, schemaError(path+".weight_permille", sid, "must be in [0,1000]")
		}
		s.WeightMyriad = PermilleToMyriad(int(wp))
		p.AppendTrace("load-v1", path+".weight_permille", TraceEntry{
			ObjectID: sid, Field: "weight_myriad", Rule: "myriad=permille*10",
			OldValue: fmt.Sprint(wp), NewValue: fmt.Sprint(s.WeightMyriad), OperandA: wp, OperandB: 10,
		})
		s.HardnessPct = DefaultHardnessPct
		p.AppendTrace("load-v1", path+".hardness", TraceEntry{
			ObjectID: sid, Field: "hardness_pct", Rule: "default-when-missing",
			OldValue: "<missing>", NewValue: fmt.Sprint(DefaultHardnessPct), SourceDefault: fmt.Sprint(DefaultHardnessPct),
		})
		dc, hasDC := obj["direction_code"]
		var code int64
		if hasDC && dc != nil {
			code, ok = asInt(dc)
			if !ok {
				return nil, schemaError(path+".direction_code", sid, "must be integer code")
			}
		}
		var dt TraceEntry
		s.DirectionClass, dt = dirFromV1(code, hasDC && dc != nil, sid, path+".direction_code")
		dt.ObjectID = sid
		p.AppendTrace("load-v1", path+".direction_code", dt)
		p.Strokes = append(p.Strokes, s)
	}
	if passes, ok := asArray(cons["passes"]); ok {
		for j, rawPass := range passes {
			pm, ok := asMap(rawPass)
			if !ok {
				continue
			}
			gid, _ := requiredString(pm, "pass_id", fmt.Sprintf("construction.passes[%d]", j), id)
			if gid == "" {
				gid = fmt.Sprintf("pass-%d", j+1)
			}
			g := StrokeGroup{ID: gid, Kind: "pass"}
			if ids, ok := asArray(pm["line_ids"]); ok {
				for _, idv := range ids {
					if sid, ok := asString(idv); ok {
						g.StrokeIDs = append(g.StrokeIDs, sid)
					}
				}
			}
			p.Groups = append(p.Groups, g)
		}
	}
	if err := parseAssets(root, p, objectID); err != nil {
		return nil, err
	}
	applySidecar(root, p)
	if err := validateLoadedProject(p); err != nil {
		return nil, err
	}
	return p, nil
}

func loadV2(root map[string]any, objectID string) (*Project, error) {
	meta, _ := asMap(root["meta"])
	id, err := requiredString(root, "projectId", "$", objectID)
	if err != nil {
		if meta != nil {
			if mid, err2 := requiredString(meta, "projectId", "meta", objectID); err2 == nil {
				id = mid
				err = nil
			}
		}
		if err != nil {
			return nil, err
		}
	}
	objectID = id
	sizeRoot := map[string]any{}
	if meta != nil {
		if size, ok := asMap(meta["size"]); ok {
			sizeRoot = size
		}
	}
	w, h, tile, err := canvasSize(map[string]any{"size": sizeRoot}, objectID, "size")
	if err != nil {
		// sizeRoot itself has w/h; canvasSize expects key map.
		w, h, tile, err = canvasSize(root, objectID, "size")
		if err != nil {
			return nil, err
		}
	}
	p := &Project{
		ID: id, SourceVersion: "v2", SchemaVersion: SchemaV2,
		Width: w, Height: h, TileWidth: tile, TileHeight: tile,
	}
	if meta != nil {
		if seed, ok := optionalIntDefault(meta, "seed", 0); ok {
			p.Seed = seed
		}
	}
	layers, ok := asArray(root["layers"])
	if !ok {
		return nil, schemaError("layers", id, "missing nested layers array")
	}
	for li, rawLayer := range layers {
		lm, ok := asMap(rawLayer)
		if !ok {
			return nil, schemaError(fmt.Sprintf("layers[%d]", li), id, "layer must be object")
		}
		lid, err := requiredString(lm, "layerId", fmt.Sprintf("layers[%d]", li), id)
		if err != nil {
			return nil, err
		}
		g := StrokeGroup{ID: lid, Kind: "layer"}
		segs, ok := asArray(lm["segments"])
		if !ok {
			return nil, schemaError(fmt.Sprintf("layers[%d].segments", li), id, "missing segments")
		}
		for si, rawSeg := range segs {
			obj, ok := asMap(rawSeg)
			if !ok {
				return nil, schemaError(fmt.Sprintf("layers[%d].segments[%d]", li, si), id, "segment must object")
			}
			sid, err := requiredString(obj, "strokeId", fmt.Sprintf("layers[%d].segments[%d]", li, si), "")
			if err != nil {
				return nil, err
			}
			base := fmt.Sprintf("layers[%d].segments[%d]", li, si)
			s := Stroke{ID: sid, Group: lid}
			seq64, err := requiredInt(obj, "strokeNo", base, sid)
			if err != nil {
				return nil, err
			}
			s.Seq = int(seq64)
			pr, _ := optionalIntDefault(obj, "priority", DefaultPriority)
			s.Priority = int(pr)
			so, err := requiredIntArray2(obj, "fromOffset", base, sid)
			if err != nil {
				return nil, err
			}
			eo, err := requiredIntArray2(obj, "toOffset", base, sid)
			if err != nil {
				return nil, err
			}
			st, en, traces, err := OffsetsToAbsolute(Point{X: int(so[0]), Y: int(so[1])}, Point{X: int(eo[0]), Y: int(eo[1])}, s.Seq, sid, DefaultCoordinateRule)
			if err != nil {
				return nil, err
			}
			s.Start, s.End = st, en
			for _, tr := range traces {
				tr.ObjectID = sid
				p.AppendTrace("load-v2", base+".offset", tr)
			}
			width, err := requiredInt(obj, "diameter", base, sid)
			if err != nil {
				return nil, err
			}
			s.Width = int(width)
			pct, err := requiredInt(obj, "weightPercent", base, sid)
			if err != nil {
				return nil, err
			}
			if pct < 0 || pct > 100 {
				return nil, schemaError(base+".weightPercent", sid, "must be [0,100]")
			}
			s.WeightMyriad = PercentToMyriad(int(pct))
			if h, ok := optionalIntDefault(obj, "hardness", DefaultHardnessPct); ok {
				s.HardnessPct = int(h)
			} else {
				s.HardnessPct = DefaultHardnessPct
				p.AppendTrace("load-v2", base+".hardness", TraceEntry{ObjectID: sid, Field: "hardness", Rule: "default-when-missing", OldValue: "<missing>", NewValue: "50", SourceDefault: "50"})
			}
			dc, _ := obj["direction"].(string)
			s.DirectionClass = dirFromV2(dc)
			p.Strokes = append(p.Strokes, s)
			g.StrokeIDs = append(g.StrokeIDs, sid)
		}
		p.Groups = append(p.Groups, g)
	}
	if err := parseAssets(root, p, objectID); err != nil {
		return nil, err
	}
	applySidecar(root, p)
	if err := validateLoadedProject(p); err != nil {
		return nil, err
	}
	return p, nil
}

func requiredIntArray2(m map[string]any, key, path, objectID string) ([]int64, error) {
	v, ok := m[key]
	if !ok {
		return nil, schemaError(path+"."+key, objectID, "missing required [x,y] field")
	}
	a, ok := asArray(v)
	if !ok || len(a) != 2 {
		return nil, schemaError(path+"."+key, objectID, "must be [x,y]")
	}
	out := make([]int64, 2)
	for i := range a {
		n, ok := asInt(a[i])
		if !ok {
			return nil, schemaError(fmt.Sprintf("%s.%s[%d]", path, key, i), objectID, "must be integer")
		}
		out[i] = n
	}
	return out, nil
}

func loadV3(root map[string]any, objectID string) (*Project, error) {
	id, err := requiredString(root, "id", "$", objectID)
	if err != nil {
		return nil, err
	}
	objectID = id
	w, h, tile, err := canvasSize(root, objectID, "canvas")
	if err != nil {
		// width/height at root accepted by canvasSize only if no canvas key.
		return nil, err
	}
	p := &Project{
		ID: id, SourceVersion: "v3", SchemaVersion: SchemaV3,
		Width: w, Height: h, TileWidth: tile, TileHeight: tile,
	}
	if seed, ok := optionalIntDefault(root, "seed", 0); ok {
		p.Seed = seed
	}
	rawStrokes, ok := asArray(root["strokes"])
	if !ok {
		return nil, schemaError("strokes", id, "missing strokes array")
	}
	for i, raw := range rawStrokes {
		obj, ok := asMap(raw)
		if !ok {
			return nil, schemaError(fmt.Sprintf("strokes[%d]", i), id, "stroke must object")
		}
		sid, err := requiredString(obj, "id", fmt.Sprintf("strokes[%d]", i), "")
		if err != nil {
			return nil, err
		}
		base := "strokes[" + sid + "]"
		s := Stroke{ID: sid}
		seq, err := requiredInt(obj, "seq", base, sid)
		if err != nil {
			return nil, err
		}
		s.Seq = int(seq)
		pr, hasPr := optionalIntDefault(obj, "priority", DefaultPriority)
		s.Priority = int(pr)
		if !hasPr {
			p.AppendTrace("load-v3", base+".priority", TraceEntry{ObjectID: sid, Field: "priority", Rule: "default-when-missing", OldValue: "<missing>", NewValue: fmt.Sprint(DefaultPriority), SourceDefault: fmt.Sprint(DefaultPriority)})
		}
		if g, ok := asString(obj["group"]); ok {
			s.Group = g
		}
		st, err := parsePointObject(obj, "start", base, sid)
		if err != nil {
			return nil, err
		}
		en, err := parsePointObject(obj, "end", base, sid)
		if err != nil {
			return nil, err
		}
		s.Start, s.End = st, en
		width, err := requiredInt(obj, "width", base, sid)
		if err != nil {
			return nil, err
		}
		s.Width = int(width)
		hard, hasHard := optionalIntDefault(obj, "hardness_pct", DefaultHardnessPct)
		s.HardnessPct = int(hard)
		if !hasHard {
			p.AppendTrace("load-v3", base+".hardness_pct", TraceEntry{ObjectID: sid, Rule: "default-when-missing", OldValue: "<missing>", NewValue: "50", SourceDefault: "50"})
		}
		weight, err := requiredInt(obj, "weight_myriad", base, sid)
		if err != nil {
			return nil, err
		}
		s.WeightMyriad = int(weight)
		dc, _ := obj["direction"].(string)
		if dc == "" {
			s.DirectionClass = DirMixed
			p.AppendTrace("load-v3", base+".direction", TraceEntry{ObjectID: sid, Rule: "default-when-missing", OldValue: "<missing>", NewValue: string(DirMixed), SourceDefault: string(DirMixed)})
		} else {
			s.DirectionClass = DirClass(dc)
		}
		p.Strokes = append(p.Strokes, s)
	}
	if groups, ok := asArray(root["stroke_groups"]); ok {
		for i, rawg := range groups {
			gm, ok := asMap(rawg)
			if !ok {
				continue
			}
			gid, err := requiredString(gm, "id", fmt.Sprintf("stroke_groups[%d]", i), id)
			if err != nil {
				return nil, err
			}
			g := StrokeGroup{ID: gid}
			if kind, ok := asString(gm["kind"]); ok {
				g.Kind = kind
			}
			if ids, ok := asArray(gm["stroke_ids"]); ok {
				for _, v := range ids {
					if sid, ok := asString(v); ok {
						g.StrokeIDs = append(g.StrokeIDs, sid)
					}
				}
			}
			p.Groups = append(p.Groups, g)
		}
	}
	if err := parseAssets(root, p, objectID); err != nil {
		return nil, err
	}
	applySidecar(root, p)
	if err := validateLoadedProject(p); err != nil {
		return nil, err
	}
	return p, nil
}

func validateLoadedProject(p *Project) error {
	if p.Width <= 0 || p.Height <= 0 {
		return schemaError("canvas", p.ID, "invalid canvas")
	}
	seen := map[string]bool{}
	for _, s := range p.Strokes {
		if seen[s.ID] {
			return schemaError("strokes.id", s.ID, "duplicate stroke id")
		}
		seen[s.ID] = true
		if err := ValidateStrokeParameters(s); err != nil {
			return err
		}
	}
	return nil
}

func parseAssets(root map[string]any, p *Project, objectID string) error {
	a, ok := asMap(root["assets"])
	if !ok {
		return nil
	}
	if cm, ok := asMap(a["candidate"]); ok {
		m, err := parseGrayAsset(cm, "assets.candidate", objectID)
		if err != nil {
			return err
		}
		p.Candidate = m
	}
	if tm, ok := asMap(a["truth_mask"]); ok {
		m, err := parseGrayAsset(tm, "assets.truth_mask", objectID)
		if err != nil {
			return err
		}
		p.Truth = BuildTruth(m, p.Strokes)
	}
	if im, ok := asMap(a["icon"]); ok {
		w, err := requiredInt(im, "width", "assets.icon", objectID)
		if err != nil {
			return err
		}
		h, err := requiredInt(im, "height", "assets.icon", objectID)
		if err != nil {
			return err
		}
		b64, err := requiredString(im, "rgba_b64", "assets.icon", objectID)
		if err != nil {
			return err
		}
		raw, derr := base64.StdEncoding.DecodeString(b64)
		if derr != nil || len(raw) != int(w*h*4) {
			return schemaError("assets.icon.rgba_b64", objectID, "invalid RGBA payload")
		}
		p.Icon = &RGBAMask{Width: int(w), Height: int(h), Pix: raw}
	}
	return nil
}

func parseGrayAsset(m map[string]any, path, objectID string) (*GrayMask, error) {
	w, err := requiredInt(m, "width", path, objectID)
	if err != nil {
		return nil, err
	}
	h, err := requiredInt(m, "height", path, objectID)
	if err != nil {
		return nil, err
	}
	b64, err := requiredString(m, "gray_b64", path, objectID)
	if err != nil {
		return nil, err
	}
	raw, derr := base64.StdEncoding.DecodeString(b64)
	if derr != nil || len(raw) != int(w*h) {
		return nil, schemaError(path+".gray_b64", objectID, "invalid grayscale payload")
	}
	return &GrayMask{Width: int(w), Height: int(h), Pix: raw}, nil
}

func grayAsset(m *GrayMask) map[string]any {
	if m == nil {
		return nil
	}
	return map[string]any{
		"width": m.Width, "height": m.Height,
		"gray_b64": base64.StdEncoding.EncodeToString(m.Pix),
		"sha256":   m.Hash(),
	}
}

func rgbaAsset(m *RGBAMask) map[string]any {
	if m == nil {
		return nil
	}
	return map[string]any{
		"width": m.Width, "height": m.Height,
		"rgba_b64": base64.StdEncoding.EncodeToString(m.Pix),
	}
}

func ExportProject(p *Project, target string) (*ExportResult, error) {
	return ExportProjectWithOptions(p, target, ExportOptions{IncludeAssets: true, IncludeAudit: true})
}

func ExportProjectWithOptions(p *Project, target string, opts ExportOptions) (*ExportResult, error) {
	traces := []TraceEntry{}
	lossy := []LossyItem{}
	residuals := map[string]int{}
	originalWeights := map[string]int{}
	sidecarDirs := map[string]string{}
	for _, s := range p.Strokes {
		originalWeights[s.ID] = s.WeightMyriad
		_, rem, tr := MyriadToPermille(s.WeightMyriad)
		if rem != 0 {
			residuals[s.ID] = rem
			traces = append(traces, tr)
		}
	}

	var doc map[string]any
	var schema string
	switch target {
	case SchemaV1, "v1", "1":
		schema = SchemaV1
		doc = exportV1(p, opts, &traces, &lossy, residuals, sidecarDirs)
	case SchemaV2, "v2", "2":
		schema = SchemaV2
		doc = exportV2(p, opts, &traces, &lossy, residuals, sidecarDirs)
	case SchemaV3, "v3", "3":
		schema = SchemaV3
		doc = exportV3(p, opts)
	default:
		return nil, schemaError("target", p.ID, "unknown export schema %q", target)
	}
	if opts.IncludeAudit {
		sidecar := map[string]any{
			"groups":     p.Groups,
			"directions": sidecarDirs,
		}
		doc["x_maskqc"] = map[string]any{
			"weight_remainders": residuals,
			"weight_myriad":     originalWeights,
			"sidecar":           sidecar,
			"lossy_report":      lossy,
			"conversion_trace":  traces,
		}
	}
	if opts.IncludeAssets {
		assets := map[string]any{}
		if a := rgbaAsset(p.Icon); a != nil {
			assets["icon"] = a
		}
		if a := grayAsset(p.Candidate); a != nil {
			assets["candidate"] = a
		}
		if p.Truth != nil {
			if a := grayAsset(p.Truth.Mask); a != nil {
				assets["truth_mask"] = a
			}
		}
		if len(assets) > 0 {
			doc["assets"] = assets
		}
	}
	data, err := json.MarshalIndent(doc, "", "  ")
	if err != nil {
		return nil, err
	}
	return &ExportResult{SchemaVersion: schema, Data: data, Trace: traces, Lossy: lossy}, nil
}

func sortedStrokesBySeq(p *Project) []Stroke {
	out := append([]Stroke(nil), p.Strokes...)
	sort.SliceStable(out, func(i, j int) bool { return out[i].Seq < out[j].Seq })
	return out
}

func exportV1(p *Project, opts ExportOptions, traces *[]TraceEntry, lossy *[]LossyItem, residuals map[string]int, sidecarDirs map[string]string) map[string]any {
	lines := []any{}
	for _, s := range sortedStrokesBySeq(p) {
		permille, rem, tr := MyriadToPermille(s.WeightMyriad)
		tr.ObjectID = s.ID
		*traces = append(*traces, tr)
		if rem != 0 {
			residuals[s.ID] = rem
		}
		code, ok := v1CodeForDir(s.DirectionClass)
		directionValue := any(nil)
		if ok {
			directionValue = code
		} else {
			sidecarDirs[s.ID] = string(s.DirectionClass)
			*lossy = append(*lossy, LossyItem{
				Path: "construction.lines[].direction_code", ObjectID: s.ID,
				Kind: "direction_category", Original: string(s.DirectionClass),
				Disposition: "omit_native_field;restore=x_maskqc.sidecar.directions",
				SidecarKey:  "x_maskqc.sidecar.directions." + s.ID,
			})
		}
		lines = append(lines, map[string]any{
			"id":               s.ID,
			"line_no":          s.Seq,
			"process_priority": s.Priority,
			"points":           []int{s.Start.X, s.Start.Y, s.End.X, s.End.Y},
			"brush_width":      s.Width,
			"weight_permille":  permille,
			"direction_code":   directionValue,
		})
	}
	passes := []any{}
	for i, g := range p.Groups {
		ids := append([]string(nil), g.StrokeIDs...)
		pass := map[string]any{"pass_id": g.ID, "pass_no": i + 1, "line_ids": ids}
		passes = append(passes, pass)
		if g.Kind != "" && g.Kind != "pass" {
			*lossy = append(*lossy, LossyItem{
				Path: fmt.Sprintf("construction.passes[%d]", i), ObjectID: g.ID,
				Kind: "group_structure_kind", Original: g.Kind,
				Disposition: "native=pass;kind_preserved=x_maskqc.sidecar.groups",
				SidecarKey:  "x_maskqc.sidecar.groups",
			})
		}
	}
	return map[string]any{
		"schema":     SchemaV1,
		"project_id": p.ID,
		"seed":       p.Seed,
		"canvas":     map[string]any{"width": p.Width, "height": p.Height, "tile": p.TileWidth},
		"construction": map[string]any{
			"lines":  lines,
			"passes": passes,
		},
	}
}

func exportV2(p *Project, opts ExportOptions, traces *[]TraceEntry, lossy *[]LossyItem, residuals map[string]int, sidecarDirs map[string]string) map[string]any {
	layers := []any{}
	groups := p.Groups
	if len(groups) == 0 {
		groups = []StrokeGroup{{ID: "all", Kind: "layer", StrokeIDs: strokeIDs(p.Strokes)}}
	}
	byID := map[string]Stroke{}
	for _, s := range p.Strokes {
		byID[s.ID] = s
	}
	for _, g := range groups {
		segs := []any{}
		for _, sid := range g.StrokeIDs {
			s, ok := byID[sid]
			if !ok {
				continue
			}
			so, eo, convTraces, err := AbsoluteToOffsets(s.Start, s.End, s.Seq, s.ID, DefaultCoordinateRule)
			if err == nil {
				*traces = append(*traces, convTraces...)
			}
			percent, rem := MyriadToPercentFloor(s.WeightMyriad)
			if rem != 0 {
				*traces = append(*traces, TraceEntry{
					ObjectID: s.ID, Field: "weightPercent", Rule: "percent=floor(myriad/100)",
					OldValue: fmt.Sprint(s.WeightMyriad), NewValue: fmt.Sprint(percent),
					TruncatedAt: "myriad/100", Remainder: int64(rem),
					OperandA: int64(s.WeightMyriad), OperandB: 100,
				})
				*lossy = append(*lossy, LossyItem{
					Path: "layers[].segments[].weightPercent", ObjectID: s.ID,
					Kind: "weight_scale_remainder", Original: fmt.Sprint(s.WeightMyriad),
					Disposition: "floor_to_percent;restore=x_maskqc.weight_myriad",
					SidecarKey:  "x_maskqc.weight_myriad." + s.ID,
				})
			}
			segs = append(segs, map[string]any{
				"strokeId":      s.ID,
				"strokeNo":      s.Seq,
				"priority":      s.Priority,
				"fromOffset":    []int{so.X, so.Y},
				"toOffset":      []int{eo.X, eo.Y},
				"diameter":      s.Width,
				"weightPercent": percent,
				"hardness":      s.HardnessPct,
				"direction":     v2CodeForDir(s.DirectionClass),
			})
		}
		layers = append(layers, map[string]any{"layerId": g.ID, "kind": g.Kind, "segments": segs})
	}
	return map[string]any{
		"schema":    SchemaV2,
		"projectId": p.ID,
		"meta":      map[string]any{"seed": p.Seed, "size": map[string]any{"w": p.Width, "h": p.Height, "tile": p.TileWidth}},
		"layers":    layers,
	}
}

func exportV3(p *Project, opts ExportOptions) map[string]any {
	strokes := []any{}
	for _, s := range sortedStrokesBySeq(p) {
		strokes = append(strokes, map[string]any{
			"id": s.ID, "seq": s.Seq, "priority": s.Priority, "group": s.Group,
			"start":         map[string]any{"x": s.Start.X, "y": s.Start.Y},
			"end":           map[string]any{"x": s.End.X, "y": s.End.Y},
			"width":         s.Width,
			"hardness_pct":  s.HardnessPct,
			"weight_myriad": s.WeightMyriad,
			"direction":     string(s.DirectionClass),
		})
	}
	groups := []any{}
	for _, g := range p.Groups {
		groups = append(groups, map[string]any{"id": g.ID, "kind": g.Kind, "stroke_ids": g.StrokeIDs})
	}
	return map[string]any{
		"schema": SchemaV3, "id": p.ID, "seed": p.Seed,
		"width": p.Width, "height": p.Height, "tile_width": p.TileWidth,
		"strokes": strokes, "stroke_groups": groups,
	}
}

func strokeIDs(strokes []Stroke) []string {
	out := make([]string, 0, len(strokes))
	for _, s := range strokes {
		out = append(out, s.ID)
	}
	return out
}
