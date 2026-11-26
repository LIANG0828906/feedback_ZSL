package maskqc

import (
	"encoding/hex"
	"encoding/json"
	"fmt"
)

const (
	DefaultHardnessPermille = 700
	DefaultDirection        = DirUnknown
	SequenceStridePixels    = 4
)

var dirToV1Code = map[DirectionClass]int{DirUnknown: 0, DirAlong: 1, DirCross: 2, DirMixed: 3}
var v1CodeToDir = map[int]DirectionClass{0: DirUnknown, 1: DirAlong, 2: DirCross, 3: DirMixed}
var dirToV2 = map[DirectionClass]string{DirUnknown: "UNKNOWN", DirAlong: "ALONG", DirCross: "CROSS", DirMixed: "MIXED"}
var v2ToDir = map[string]DirectionClass{"UNKNOWN": DirUnknown, "ALONG": DirAlong, "CROSS": DirCross, "MIXED": DirMixed}

func ExportProject(p *Project, version string) ([]byte, *ConvertReport, error) {
	rep := &ConvertReport{FromSchema: p.Schema, ToSchema: version}
	var doc map[string]any
	var err error
	switch version {
	case SchemaV1:
		doc, err = exportV1(p, rep)
	case SchemaV2:
		doc, err = exportV2(p, rep)
	case SchemaV3:
		doc, err = exportV3(p, rep)
	default:
		return nil, nil, newError(ESchema, "schema", "unknown target schema %q", version)
	}
	if err != nil {
		return nil, rep, err
	}
	if len(rep.Lossy) > 0 {
		rep.Code = ELossy
		for i := range rep.Lossy {
			rep.Lossy[i].Code = ELossy
		}
	}
	data, err := json.MarshalIndent(doc, "", "  ")
	if err != nil {
		return nil, rep, err
	}
	return data, rep, nil
}

func (r ConvertReport) LossyError() error {
	if len(r.Lossy) == 0 {
		return nil
	}
	return &Error{Code: ELossy, FieldPath: "export", Message: fmt.Sprintf("%d lossy conversion item(s)", len(r.Lossy)), Details: map[string]any{"items": r.Lossy}}
}

func exportCommonTiles(ic *Icon) []any {
	tiles := make([]any, 0, len(ic.TruthTiles))
	for _, t := range ic.TruthTiles {
		tiles = append(tiles, map[string]any{
			"tile_id": t.ID, "x": t.Rect.MinX / ic.TileWidth, "y": t.Rect.MinY / ic.TileHeight,
			"rect": []int{t.Rect.MinX, t.Rect.MinY, t.Rect.MaxX, t.Rect.MaxY}, "sha256": t.Hash,
		})
	}
	return tiles
}

func exportV1(p *Project, rep *ConvertReport) (map[string]any, error) {
	images := make([]any, 0, len(p.Icons))
	for _, ic := range p.Icons {
		strokes := make([]any, 0, len(ic.Strokes))
		for _, s := range ic.Strokes {
			permille, rem, tr, err := ScalePermyriadToPermille(s.WeightPermyriad, fmt.Sprintf("images.%s.strokes.%s.opacity_permille", ic.ID, s.ID))
			if err != nil {
				return nil, err
			}
			rep.Traces = append(rep.Traces, tr)
			strokes = append(strokes, map[string]any{
				"stroke_id": s.ID, "pen_no": s.Seq, "proc_priority": s.Priority,
				"x1": s.Start.X, "y1": s.Start.Y, "x2": s.End.X, "y2": s.End.Y,
				"brush_radius": s.Radius, "opacity_permille": permille, "weight_residual_bp": rem,
				"dir_code": dirToV1Code[s.DeclaredDirection],
			})
			rep.Lossy = append(rep.Lossy,
				LossyItem{StrokeID: s.ID, Path: "hardness", Feature: "brush_hardness_permille", Action: "default_on_v1_reload", Detail: fmt.Sprintf("internal=%d; v1 has no hardness field", s.HardnessPermille)},
				LossyItem{StrokeID: s.ID, Path: "group", Feature: "stroke_group", Action: "omit_field", Detail: s.Group},
				LossyItem{StrokeID: s.ID, Path: "composition_order", Feature: "composition_order", Action: "omit_field_default_on_reload", Detail: fmt.Sprintf("internal=%d", s.CompositionOrder)},
			)
			if s.DeclaredDirection == DirMixed {
				rep.Lossy = append(rep.Lossy, LossyItem{StrokeID: s.ID, Path: "dir_code", Feature: "mixed_direction_annotation", Action: "encode_numeric_3", Detail: "v1 retains only coarse code"})
			}
		}
		images = append(images, map[string]any{
			"image_id": ic.ID, "width": ic.Width, "height": ic.Height,
			"tile_w": ic.TileWidth, "tile_h": ic.TileHeight,
			"candidate_mask_hex": hex.EncodeToString(ic.Candidate.Alpha),
			"truth_mask_hex":     hex.EncodeToString(ic.Truth.Alpha),
			"truth_blocks":       exportCommonTiles(ic),
			"strokes":            strokes,
		})
	}
	return map[string]any{"schema": SchemaV1, "seed": p.Seed, "project_name": p.Name, "images": images}, nil
}

func exportV2(p *Project, rep *ConvertReport) (map[string]any, error) {
	icons := make([]any, 0, len(p.Icons))
	for _, ic := range p.Icons {
		strokes := make([]any, 0, len(ic.Strokes))
		for _, s := range ic.Strokes {
			wPermille, rem, wtr, err := ScalePermyriadToPermille(s.WeightPermyriad, fmt.Sprintf("icons.%s.strokes.%s.coverage.permille", ic.ID, s.ID))
			if err != nil {
				return nil, err
			}
			rep.Traces = append(rep.Traces, wtr)
			sdx, _, ctr1, err := AbsoluteToSequenceOffset(s.Start.X, s.Seq, SequenceStridePixels, "startDX")
			if err != nil {
				return nil, err
			}
			sdy, _, ctr2, err := AbsoluteToSequenceOffset(s.Start.Y, s.Seq, SequenceStridePixels, "startDY")
			if err != nil {
				return nil, err
			}
			edx, _, ctr3, err := AbsoluteToSequenceOffset(s.End.X, s.Seq, SequenceStridePixels, "endDX")
			if err != nil {
				return nil, err
			}
			edy, _, ctr4, err := AbsoluteToSequenceOffset(s.End.Y, s.Seq, SequenceStridePixels, "endDY")
			if err != nil {
				return nil, err
			}
			rep.Traces = append(rep.Traces, ctr1, ctr2, ctr3, ctr4)
			hardnessPct := s.HardnessPermille / 10
			strokes = append(strokes, map[string]any{
				"strokeId": s.ID, "penSequence": s.Seq, "phasePriority": s.Priority,
				"groupName": s.Group, "coordStride": SequenceStridePixels,
				"startDX": sdx, "startDY": sdy, "endDX": edx, "endDY": edy,
				"brushRadius": s.Radius, "hardnessPct": hardnessPct,
				"coverage":  map[string]any{"permille": wPermille, "weightResidualBP": rem},
				"direction": dirToV2[s.DeclaredDirection],
				"orderHint": s.CompositionOrder,
			})
		}
		icons = append(icons, map[string]any{
			"iconId": ic.ID, "width": ic.Width, "height": ic.Height,
			"tileWidth": ic.TileWidth, "tileHeight": ic.TileHeight,
			"candidateMaskHex": hex.EncodeToString(ic.Candidate.Alpha),
			"truthMaskHex":     hex.EncodeToString(ic.Truth.Alpha),
			"truthTiles":       exportCommonTiles(ic),
			"strokes":          strokes,
		})
		rep.Lossy = append(rep.Lossy, LossyItem{Path: fmt.Sprintf("icons.%s.stroke_groups", ic.ID), Feature: "ordered_group_structure", Action: "flatten_to_groupName", Detail: "v2 preserves only a flat group name"})
	}
	return map[string]any{"schemaVersion": SchemaV2, "fixedSeed": p.Seed, "name": p.Name, "icons": icons}, nil
}

func exportV3(p *Project, rep *ConvertReport) (map[string]any, error) {
	icons := make([]any, 0, len(p.Icons))
	groups := map[string][]any{}
	for _, ic := range p.Icons {
		strokes := make([]any, 0, len(ic.Strokes))
		for _, s := range ic.Strokes {
			strokes = append(strokes, map[string]any{
				"id": s.ID, "seq": s.Seq, "process_priority": s.Priority, "group_id": s.Group,
				"start":             map[string]any{"x": s.Start.X, "y": s.Start.Y},
				"end":               map[string]any{"x": s.End.X, "y": s.End.Y},
				"brush":             map[string]any{"radius": s.Radius, "hardness_permille": s.HardnessPermille},
				"coverage":          map[string]any{"weight_permyriad": s.WeightPermyriad},
				"direction":         map[string]any{"class": string(s.DeclaredDirection)},
				"composition_order": s.CompositionOrder,
			})
			if s.Group != "" {
				groups[s.Group] = append(groups[s.Group], s.ID)
			}
		}
		icons = append(icons, map[string]any{
			"id": ic.ID, "width": ic.Width, "height": ic.Height,
			"tile_width": ic.TileWidth, "tile_height": ic.TileHeight,
			"candidate_mask_hex": hex.EncodeToString(ic.Candidate.Alpha),
			"truth_mask_hex":     hex.EncodeToString(ic.Truth.Alpha),
			"truth_tiles":        exportCommonTiles(ic),
			"strokes":            strokes,
		})
	}
	groupDocs := make([]any, 0, len(groups))
	for id, ids := range groups {
		groupDocs = append(groupDocs, map[string]any{"id": id, "mode": "declared-order", "stroke_ids": ids})
	}
	return map[string]any{"schema": SchemaV3, "seed": p.Seed, "name": p.Name, "stroke_groups": groupDocs, "icons": icons}, nil
}

func LoadProject(data []byte) (*Project, error) {
	var doc map[string]any
	if err := json.Unmarshal(data, &doc); err != nil {
		return nil, newError(ESchema, "$", "invalid JSON: %v", err)
	}
	schema, _ := doc["schema"].(string)
	if schema == "" {
		schema, _ = doc["schemaVersion"].(string)
	}
	if schema == "" {
		return nil, newError(ESchema, "schema", "missing required schema version")
	}
	switch schema {
	case SchemaV1:
		return loadV1(doc, data)
	case SchemaV2:
		return loadV2(doc, data)
	case SchemaV3:
		return loadV3(doc, data)
	default:
		return nil, newError(ESchema, "schema", "unknown schema version %q", schema)
	}
}

func mustMap(v any, path string) (map[string]any, error) {
	m, ok := v.(map[string]any)
	if !ok {
		return nil, newError(ESchema, path, "expected object")
	}
	return m, nil
}

func reqString(m map[string]any, path, key string) (string, error) {
	v, ok := m[key]
	if !ok || v == nil {
		return "", newError(ESchema, path+"."+key, "missing required field")
	}
	s, ok := v.(string)
	if !ok || s == "" {
		return "", newError(ESchema, path+"."+key, "expected non-empty string")
	}
	return s, nil
}

func reqInt(m map[string]any, path, key string) (int, error) {
	v, ok := m[key]
	if !ok || v == nil {
		return 0, newError(ESchema, path+"."+key, "missing required field")
	}
	f, ok := v.(float64)
	if !ok || f != float64(int(f)) {
		return 0, newError(ESchema, path+"."+key, "expected integer")
	}
	return int(f), nil
}

func optInt(m map[string]any, key string, def int) (int, bool) {
	v, ok := m[key]
	if !ok || v == nil {
		return def, false
	}
	f, ok := v.(float64)
	if !ok {
		return def, false
	}
	return int(f), true
}

func reqArray(m map[string]any, path, key string) ([]any, error) {
	v, ok := m[key]
	if !ok || v == nil {
		return nil, newError(ESchema, path+"."+key, "missing required field")
	}
	a, ok := v.([]any)
	if !ok {
		return nil, newError(ESchema, path+"."+key, "expected array")
	}
	return a, nil
}

func decodeMaskHex(s string, w, h int, path string) (*Mask, error) {
	raw, err := hex.DecodeString(s)
	if err != nil {
		return nil, newError(ESchema, path, "mask is not valid hex: %v", err)
	}
	m := &Mask{Width: w, Height: h, Alpha: raw}
	if err := m.Validate(); err != nil {
		return nil, newError(ESchema, path, "%v", err)
	}
	return m, nil
}

func baseProject(doc map[string]any, schema string, raw []byte) *Project {
	seed, _ := optInt(doc, "seed", int(DefaultSeed))
	if schema == SchemaV2 {
		seed, _ = optInt(doc, "fixedSeed", int(DefaultSeed))
	}
	name, _ := doc["name"].(string)
	if name == "" {
		name, _ = doc["project_name"].(string)
	}
	return &Project{Schema: schema, Seed: int64(seed), Name: name, SourceRaw: raw}
}

func loadTruthTiles(icon *Icon, raw any, path string) error {
	arr, ok := raw.([]any)
	if !ok {
		return newError(ESchema, path, "missing truth tile list")
	}
	for i, item := range arr {
		m, err := mustMap(item, fmt.Sprintf("%s[%d]", path, i))
		if err != nil {
			return err
		}
		id, err := reqString(m, fmt.Sprintf("%s[%d]", path, i), "tile_id")
		if err != nil {
			return err
		}
		hash, err := reqString(m, fmt.Sprintf("%s[%d]", path, i), "sha256")
		if err != nil {
			return err
		}
		rectArr, err := reqArray(m, fmt.Sprintf("%s[%d]", path, i), "rect")
		if err != nil {
			return err
		}
		if len(rectArr) != 4 {
			return newError(ESchema, fmt.Sprintf("%s[%d].rect", path, i), "must have four coordinates")
		}
		coords := make([]int, 4)
		for j := range coords {
			f, ok := rectArr[j].(float64)
			if !ok {
				return newError(ESchema, fmt.Sprintf("%s[%d].rect[%d]", path, i, j), "expected integer")
			}
			coords[j] = int(f)
		}
		icon.TruthTiles = append(icon.TruthTiles, TileTruth{ID: id, IconID: icon.ID, Hash: hash, Rect: Rect{coords[0], coords[1], coords[2], coords[3]}})
	}
	return nil
}
