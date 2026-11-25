package schemaio

import (
	"bytes"
	"encoding/json"
	"fmt"
	"math"

	"maskqa/internal/model"
)

const (
	V1 = "v1"
	V2 = "v2"
	V3 = "v3"

	DefaultHardnessBP = 8000
	WeightToleranceV1 = 50
	WeightToleranceV2 = 21
)

type Engineering struct {
	SchemaVersion string
	ProjectID     string
	Seed          int64
	Width         int
	Height        int
	ChunkSize     int
	Strokes       []model.Stroke
}

type Envelope struct {
	Schema             string `json:"schema"`
	SchemaVersion      string `json:"schema_version"`
	SchemaVersionCamel string `json:"schemaVersion"`
}

func DetectVersion(raw []byte) (string, error) {
	var env Envelope
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.UseNumber()
	if err := dec.Decode(&env); err != nil {
		return "", model.NewError(model.CodeSchema, "$", "", "invalid JSON: %v", err)
	}
	switch {
	case env.Schema == "icon-project/1.0":
		return V1, nil
	case env.SchemaVersionCamel == "v2" || env.SchemaVersionCamel == "2.0":
		return V2, nil
	case env.SchemaVersion == "v3" || env.SchemaVersion == "3.0":
		return V3, nil
	case env.SchemaVersion == "v1" || env.Schema == "v1":
		return V1, nil
	default:
		v := env.SchemaVersion
		if v == "" {
			v = env.SchemaVersionCamel
		}
		if v == "" {
			v = env.Schema
		}
		return "", &model.Error{Code: model.CodeSchema, Field: "schema_version", Message: "unknown schema version", Original: v}
	}
}

func LoadEngineering(raw []byte) (*Engineering, error) {
	version, err := DetectVersion(raw)
	if err != nil {
		return nil, err
	}
	var root map[string]any
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.UseNumber()
	if err := dec.Decode(&root); err != nil {
		return nil, model.NewError(model.CodeSchema, "$", "", "invalid JSON object: %v", err)
	}
	switch version {
	case V1:
		return loadV1(root)
	case V2:
		return loadV2(root)
	default:
		return loadV3(root)
	}
}

func asObject(v any, path string) (map[string]any, error) {
	m, ok := v.(map[string]any)
	if !ok {
		return nil, model.NewError(model.CodeSchema, path, "", "expected object")
	}
	return m, nil
}

func requiredString(obj map[string]any, path, key string) (string, error) {
	v, ok := obj[key]
	if !ok || v == nil {
		return "", model.NewError(model.CodeSchema, path+"."+key, "", "required field is missing")
	}
	s, ok := v.(string)
	if !ok || s == "" {
		return "", model.NewError(model.CodeSchema, path+"."+key, "", "expected non-empty string")
	}
	return s, nil
}

func requiredInt(obj map[string]any, path, key string) (int, error) {
	v, ok := obj[key]
	if !ok || v == nil {
		return 0, model.NewError(model.CodeSchema, path+"."+key, "", "required field is missing")
	}
	n, err := anyInt(v)
	if err != nil {
		return 0, model.NewError(model.CodeSchema, path+"."+key, "", "expected integer: %v", err)
	}
	return n, nil
}

func optionalInt(obj map[string]any, key string) (int, bool, error) {
	v, ok := obj[key]
	if !ok || v == nil {
		return 0, false, nil
	}
	n, err := anyInt(v)
	if err != nil {
		return 0, false, model.NewError(model.CodeSchema, key, "", "expected integer: %v", err)
	}
	return n, true, nil
}

func anyInt(v any) (int, error) {
	switch n := v.(type) {
	case json.Number:
		i, err := n.Int64()
		if err != nil {
			f, ferr := n.Float64()
			if ferr != nil || math.Trunc(f) != f {
				return 0, fmt.Errorf("not an integer")
			}
			return int(f), nil
		}
		return int(i), nil
	case int:
		return n, nil
	case int64:
		return int(n), nil
	case float64:
		if math.Trunc(n) != n {
			return 0, fmt.Errorf("not an integer")
		}
		return int(n), nil
	default:
		return 0, fmt.Errorf("unsupported number type %T", v)
	}
}

func requiredArray(obj map[string]any, path, key string) ([]any, error) {
	v, ok := obj[key]
	if !ok || v == nil {
		return nil, model.NewError(model.CodeSchema, path+"."+key, "", "required field is missing")
	}
	a, ok := v.([]any)
	if !ok {
		return nil, model.NewError(model.CodeSchema, path+"."+key, "", "expected array")
	}
	return a, nil
}

func requiredPointPair(obj map[string]any, path, key string) (model.Point, error) {
	a, err := requiredArray(obj, path, key)
	if err != nil {
		return model.Point{}, err
	}
	if len(a) != 2 {
		return model.Point{}, model.NewError(model.CodeSchema, path+"."+key, "", "point must have exactly two integers")
	}
	x, err := anyInt(a[0])
	if err != nil {
		return model.Point{}, model.NewError(model.CodeSchema, path+"."+key+"[0]", "", "expected integer")
	}
	y, err := anyInt(a[1])
	if err != nil {
		return model.Point{}, model.NewError(model.CodeSchema, path+"."+key+"[1]", "", "expected integer")
	}
	return model.Point{X: x, Y: y}, nil
}

func baseEngineering(root map[string]any, version string) (*Engineering, error) {
	idKey := "id"
	seedKey := "seed"
	widthKey := "width"
	heightKey := "height"
	switch version {
	case V1:
		idKey = "project_id"
	case V2:
		idKey = "projectId"
	}
	id, err := requiredString(root, "$", idKey)
	if err != nil {
		return nil, err
	}
	width, err := requiredInt(root, "$", widthKey)
	if err != nil {
		return nil, err
	}
	height, err := requiredInt(root, "$", heightKey)
	if err != nil {
		return nil, err
	}
	seed, _, _ := optionalInt(root, seedKey)
	chunkSize, ok, _ := optionalInt(root, "chunk_size")
	if !ok {
		chunkSize = 16
	}
	return &Engineering{SchemaVersion: version, ProjectID: id, Seed: int64(seed), Width: width, Height: height, ChunkSize: chunkSize}, nil
}

func loadV1(root map[string]any) (*Engineering, error) {
	e, err := baseEngineering(root, V1)
	if err != nil {
		return nil, err
	}
	rawStrokes, err := requiredArray(root, "$", "strokes")
	if err != nil {
		return nil, err
	}
	for i, raw := range rawStrokes {
		path := fmt.Sprintf("$.strokes[%d]", i)
		obj, err := asObject(raw, path)
		if err != nil {
			return nil, err
		}
		id, err := requiredString(obj, path, "stroke_id")
		if err != nil {
			return nil, err
		}
		no, err := requiredInt(obj, path, "stroke_no")
		if err != nil {
			return nil, err
		}
		priority, err := requiredInt(obj, path, "process_priority")
		if err != nil {
			return nil, err
		}
		x1, err := requiredInt(obj, path, "start_x")
		if err != nil {
			return nil, err
		}
		y1, err := requiredInt(obj, path, "start_y")
		if err != nil {
			return nil, err
		}
		x2, err := requiredInt(obj, path, "end_x")
		if err != nil {
			return nil, err
		}
		y2, err := requiredInt(obj, path, "end_y")
		if err != nil {
			return nil, err
		}
		pct, err := requiredInt(obj, path, "coverage_pct")
		if err != nil {
			return nil, err
		}
		if pct < 0 || pct > 100 {
			return nil, model.NewError(model.CodePara, "coverage_pct", id, "source weight %d outside declared scale 0..100", pct)
		}
		hardPct, hasHard, err := optionalInt(obj, "hardness_pct")
		if err != nil {
			return nil, err
		}
		if hasHard && (hardPct < 0 || hardPct > 100) {
			return nil, model.NewError(model.CodePara, "hardness_pct", id, "source hardness %d outside declared scale 0..100", hardPct)
		}
		widthPx, hasWidth, err := optionalInt(obj, "brush_size")
		if err != nil {
			return nil, err
		}
		if !hasWidth {
			widthPx = 1
		}
		weight := ScaleNonNegative(pct, 100, model.WeightScaleBP)
		hardness := DefaultHardnessBP
		if hasHard {
			hardness = ScaleNonNegative(hardPct, 100, model.HardnessScaleBP)
		}
		s := model.Stroke{
			ID: id, No: no, Priority: priority,
			From: model.Point{X: x1, Y: y1}, To: model.Point{X: x2, Y: y2},
			WidthPx: widthPx, WeightBP: weight, HardnessBP: hardness, HasHardness: hasHard,
			DeclaredIndex: i, SourceVersion: V1, ParamVersion: model.ParamsVersion,
			Traces: []model.FieldTrace{
				{Field: "From.X", SourcePath: path + ".start_x", SourceValue: x1, InternalField: "stroke.from.x", InternalValue: x1, Conversion: "absolute_pixel_identity"},
				{Field: "From.Y", SourcePath: path + ".start_y", SourceValue: y1, InternalField: "stroke.from.y", InternalValue: y1, Conversion: "absolute_pixel_identity"},
				{Field: "To.X", SourcePath: path + ".end_x", SourceValue: x2, InternalField: "stroke.to.x", InternalValue: x2, Conversion: "absolute_pixel_identity"},
				{Field: "To.Y", SourcePath: path + ".end_y", SourceValue: y2, InternalField: "stroke.to.y", InternalValue: y2, Conversion: "absolute_pixel_identity"},
				{Field: "WeightBP", SourcePath: path + ".coverage_pct", SourceValue: pct, InternalField: "stroke.weight_bp", InternalValue: weight, Conversion: "round_half_up_scale_100_to_10000", Residual: weight - pct*100},
				{Field: "HardnessBP", SourcePath: path + ".hardness_pct", SourceValue: hardnessSourceValue(hasHard, hardPct), InternalField: "stroke.hardness_bp", InternalValue: hardness, Conversion: hardnessConversion(hasHard, "round_half_up_scale_100_to_10000")},
				{Field: "WidthPx", SourcePath: path + ".brush_size", SourceValue: widthPx, InternalField: "stroke.width_px", InternalValue: widthPx, Conversion: "integer_identity_default_1_when_absent"},
			},
		}
		s.ContentHash = StrokeHash(s)
		e.Strokes = append(e.Strokes, s)
	}
	return e, nil
}

func loadV2(root map[string]any) (*Engineering, error) {
	e, err := baseEngineering(root, V2)
	if err != nil {
		return nil, err
	}
	rawStrokes, err := requiredArray(root, "$", "strokeList")
	if err != nil {
		return nil, err
	}
	for i, raw := range rawStrokes {
		path := fmt.Sprintf("$.strokeList[%d]", i)
		obj, err := asObject(raw, path)
		if err != nil {
			return nil, err
		}
		id, err := requiredString(obj, path, "strokeId")
		if err != nil {
			return nil, err
		}
		no, err := requiredInt(obj, path, "strokeOrder")
		if err != nil {
			return nil, err
		}
		priority, err := requiredInt(obj, path, "priorityLevel")
		if err != nil {
			return nil, err
		}
		ox, err := requiredInt(obj, path, "originX")
		if err != nil {
			return nil, err
		}
		oy, err := requiredInt(obj, path, "originY")
		if err != nil {
			return nil, err
		}
		dx, err := requiredInt(obj, path, "deltaX")
		if err != nil {
			return nil, err
		}
		dy, err := requiredInt(obj, path, "deltaY")
		if err != nil {
			return nil, err
		}
		w255, err := requiredInt(obj, path, "weight255")
		if err != nil {
			return nil, err
		}
		if w255 < 0 || w255 > 255 {
			return nil, model.NewError(model.CodePara, "weight255", id, "source weight %d outside declared scale 0..255", w255)
		}
		widthPx, hasWidth, err := optionalInt(obj, "brushSize")
		if err != nil {
			return nil, err
		}
		if !hasWidth {
			widthPx = 1
		}
		weight := ScaleNonNegative(w255, 255, model.WeightScaleBP)
		from := model.Point{X: ox, Y: oy}
		to := AbsoluteFromRelative(from, model.Point{X: dx, Y: dy})
		s := model.Stroke{
			ID: id, No: no, Priority: priority, From: from, To: to,
			WidthPx: widthPx, WeightBP: weight, HardnessBP: DefaultHardnessBP, HasHardness: false,
			DeclaredIndex: i, SourceVersion: V2, ParamVersion: model.ParamsVersion,
			Traces: []model.FieldTrace{
				{Field: "From.X", SourcePath: path + ".originX", SourceValue: ox, InternalField: "stroke.from.x", InternalValue: ox, Conversion: "absolute_origin_identity"},
				{Field: "From.Y", SourcePath: path + ".originY", SourceValue: oy, InternalField: "stroke.from.y", InternalValue: oy, Conversion: "absolute_origin_identity"},
				{Field: "To.X", SourcePath: path + ".deltaX", SourceValue: dx, InternalField: "stroke.to.x", InternalValue: to.X, Conversion: "relative_delta_add_origin_x"},
				{Field: "To.Y", SourcePath: path + ".deltaY", SourceValue: dy, InternalField: "stroke.to.y", InternalValue: to.Y, Conversion: "relative_delta_add_origin_y"},
				{Field: "WeightBP", SourcePath: path + ".weight255", SourceValue: w255, InternalField: "stroke.weight_bp", InternalValue: weight, Conversion: "round_half_up_scale_255_to_10000"},
				{Field: "HardnessBP", SourcePath: path + ".hardness", InternalField: "stroke.hardness_bp", InternalValue: DefaultHardnessBP, Conversion: "field_absent_use_declared_default_8000bp"},
				{Field: "WidthPx", SourcePath: path + ".brushSize", SourceValue: widthPx, InternalField: "stroke.width_px", InternalValue: widthPx, Conversion: "integer_identity_default_1_when_absent"},
			},
		}
		s.ContentHash = StrokeHash(s)
		e.Strokes = append(e.Strokes, s)
	}
	return e, nil
}

func loadV3(root map[string]any) (*Engineering, error) {
	e, err := baseEngineering(root, V3)
	if err != nil {
		return nil, err
	}
	if _, ok := root["chunk_size"]; !ok {
		e.ChunkSize = 16
	}
	rawStrokes, err := requiredArray(root, "$", "strokes")
	if err != nil {
		return nil, err
	}
	for i, raw := range rawStrokes {
		path := fmt.Sprintf("$.strokes[%d]", i)
		obj, err := asObject(raw, path)
		if err != nil {
			return nil, err
		}
		id, err := requiredString(obj, path, "id")
		if err != nil {
			return nil, err
		}
		no, err := requiredInt(obj, path, "stroke_no")
		if err != nil {
			return nil, err
		}
		priority, err := requiredInt(obj, path, "priority")
		if err != nil {
			return nil, err
		}
		from, err := requiredPointPair(obj, path, "start_xy")
		if err != nil {
			return nil, err
		}
		to, err := requiredPointPair(obj, path, "end_xy")
		if err != nil {
			return nil, err
		}
		weight, err := requiredInt(obj, path, "weight_bp")
		if err != nil {
			return nil, err
		}
		hardness, err := requiredInt(obj, path, "hardness_bp")
		if err != nil {
			return nil, err
		}
		if weight < 0 || weight > model.WeightScaleBP {
			return nil, model.NewError(model.CodePara, "weight_bp", id, "source weight %d outside declared scale 0..%d", weight, model.WeightScaleBP)
		}
		if hardness < 0 || hardness > model.HardnessScaleBP {
			return nil, model.NewError(model.CodePara, "hardness_bp", id, "source hardness %d outside declared scale 0..%d", hardness, model.HardnessScaleBP)
		}
		widthPx, hasWidth, err := optionalInt(obj, "width_px")
		if err != nil {
			return nil, err
		}
		if !hasWidth {
			widthPx = 1
		}
		s := model.Stroke{
			ID: id, No: no, Priority: priority, From: from, To: to,
			WidthPx: widthPx, WeightBP: weight, HardnessBP: hardness, HasHardness: true,
			DeclaredIndex: i, SourceVersion: V3, ParamVersion: model.ParamsVersion,
			Traces: []model.FieldTrace{
				{Field: "From", SourcePath: path + ".start_xy", SourceValue: []int{from.X, from.Y}, InternalField: "stroke.from", InternalValue: from, Conversion: "absolute_pixel_integer_pair"},
				{Field: "To", SourcePath: path + ".end_xy", SourceValue: []int{to.X, to.Y}, InternalField: "stroke.to", InternalValue: to, Conversion: "absolute_pixel_integer_pair"},
				{Field: "WeightBP", SourcePath: path + ".weight_bp", SourceValue: weight, InternalField: "stroke.weight_bp", InternalValue: weight, Conversion: "canonical_identity"},
				{Field: "HardnessBP", SourcePath: path + ".hardness_bp", SourceValue: hardness, InternalField: "stroke.hardness_bp", InternalValue: hardness, Conversion: "canonical_identity"},
				{Field: "WidthPx", SourcePath: path + ".width_px", SourceValue: widthPx, InternalField: "stroke.width_px", InternalValue: widthPx, Conversion: "integer_identity_default_1_when_absent"},
			},
		}
		s.ContentHash = StrokeHash(s)
		e.Strokes = append(e.Strokes, s)
	}
	return e, nil
}

func ScaleNonNegative(value, fromScale, toScale int) int {
	if value < 0 {
		value = 0
	}
	if value > fromScale {
		value = fromScale
	}
	n := int64(value) * int64(toScale)
	d := int64(fromScale)
	return int((n + d/2) / d)
}

func RelativeOffset(origin, p model.Point) model.Point {
	return model.Point{X: p.X - origin.X, Y: p.Y - origin.Y}
}

func AbsoluteFromRelative(origin, delta model.Point) model.Point {
	return model.Point{X: origin.X + delta.X, Y: origin.Y + delta.Y}
}

func StrokeHash(s model.Stroke) string {
	type stable struct {
		ID       string
		No       int
		Priority int
		Width    int
		FX, FY   int
		TX, TY   int
		Weight   int
		Hardness int
		HasHard  bool
		PV       string
	}
	return model.HashCanonical(stable{s.ID, s.No, s.Priority, s.WidthPx, s.From.X, s.From.Y, s.To.X, s.To.Y, s.WeightBP, s.HardnessBP, s.HasHardness, s.ParamVersion})
}

func MarshalDocument(e *Engineering, version string) ([]byte, error) {
	switch version {
	case V1:
		doc := map[string]any{
			"schema": "icon-project/1.0", "project_id": e.ProjectID, "seed": e.Seed,
			"width": e.Width, "height": e.Height, "chunk_size": e.ChunkSize,
		}
		var ss []any
		for _, s := range e.Strokes {
			m := map[string]any{
				"stroke_id": s.ID, "stroke_no": s.No, "process_priority": s.Priority,
				"start_x": s.From.X, "start_y": s.From.Y, "end_x": s.To.X, "end_y": s.To.Y,
				"coverage_pct": ScaleNonNegative(s.WeightBP, model.WeightScaleBP, 100),
				"brush_size":   s.WidthPx,
			}
			if s.HasHardness {
				m["hardness_pct"] = ScaleNonNegative(s.HardnessBP, model.HardnessScaleBP, 100)
			}
			ss = append(ss, m)
		}
		doc["strokes"] = ss
		return json.MarshalIndent(doc, "", "  ")
	case V2:
		doc := map[string]any{
			"schemaVersion": "v2", "projectId": e.ProjectID, "seed": e.Seed,
			"width": e.Width, "height": e.Height, "chunk_size": e.ChunkSize,
		}
		var ss []any
		for _, s := range e.Strokes {
			delta := RelativeOffset(s.From, s.To)
			ss = append(ss, map[string]any{
				"strokeId": s.ID, "strokeOrder": s.No, "priorityLevel": s.Priority,
				"originX": s.From.X, "originY": s.From.Y,
				"deltaX": delta.X, "deltaY": delta.Y,
				"weight255": ScaleNonNegative(s.WeightBP, model.WeightScaleBP, 255),
				"brushSize": s.WidthPx,
			})
		}
		doc["strokeList"] = ss
		return json.MarshalIndent(doc, "", "  ")
	default:
		doc := map[string]any{
			"schema_version": "v3", "id": e.ProjectID, "seed": e.Seed,
			"width": e.Width, "height": e.Height, "chunk_size": e.ChunkSize,
		}
		var ss []any
		for _, s := range e.Strokes {
			ss = append(ss, map[string]any{
				"id": s.ID, "stroke_no": s.No, "priority": s.Priority,
				"start_xy": []int{s.From.X, s.From.Y}, "end_xy": []int{s.To.X, s.To.Y},
				"width_px": s.WidthPx, "weight_bp": s.WeightBP, "hardness_bp": s.HardnessBP,
			})
		}
		doc["strokes"] = ss
		return json.MarshalIndent(doc, "", "  ")
	}
}

type RoundTripReport struct {
	TargetVersion     string       `json:"target_version"`
	StrokeID          string       `json:"stroke_id"`
	Original          model.Stroke `json:"original"`
	Returned          model.Stroke `json:"returned"`
	WeightResidualBP  int          `json:"weight_residual_bp"`
	WeightToleranceBP int          `json:"weight_tolerance_bp"`
	OK                bool         `json:"ok"`
	Error             *model.Error `json:"error,omitempty"`
}

func VerifyRoundTrip(original model.Stroke, targetVersion string, meta Engineering) RoundTripReport {
	meta.SchemaVersion = V3
	meta.Strokes = []model.Stroke{original}
	raw, err := MarshalDocument(&meta, targetVersion)
	rep := RoundTripReport{TargetVersion: targetVersion, StrokeID: original.ID, Original: original}
	if err != nil {
		rep.Error = model.NewError(model.CodeRound, "document", original.ID, "encode failed: %v", err)
		return rep
	}
	loaded, err := LoadEngineering(raw)
	if err != nil {
		if me, ok := err.(*model.Error); ok {
			rep.Error = me
		} else {
			rep.Error = model.NewError(model.CodeRound, "document", original.ID, "%v", err)
		}
		return rep
	}
	if len(loaded.Strokes) != 1 {
		rep.Error = model.NewError(model.CodeRound, "strokes", original.ID, "expected one stroke after round trip")
		return rep
	}
	got := loaded.Strokes[0]
	rep.Returned = got
	if got.No != original.No {
		rep.Error = &model.Error{Code: model.CodeRound, Field: "stroke_no", StrokeID: original.ID, Message: "stroke number did not round-trip", Original: original.No, Residual: got.No - original.No}
		return rep
	}
	if got.From != original.From {
		rep.Error = &model.Error{Code: model.CodeRound, Field: "from", StrokeID: original.ID, Message: "start coordinate did not round-trip", Original: original.From, Residual: model.Point{X: got.From.X - original.From.X, Y: got.From.Y - original.From.Y}}
		return rep
	}
	if got.To != original.To {
		rep.Error = &model.Error{Code: model.CodeRound, Field: "to", StrokeID: original.ID, Message: "end coordinate did not round-trip", Original: original.To, Residual: model.Point{X: got.To.X - original.To.X, Y: got.To.Y - original.To.Y}}
		return rep
	}
	tol := WeightToleranceV1
	if targetVersion == V2 {
		tol = WeightToleranceV2
	}
	if targetVersion == V3 {
		tol = 0
	}
	residual := absInt(got.WeightBP - original.WeightBP)
	rep.WeightResidualBP = residual
	rep.WeightToleranceBP = tol
	if residual > tol {
		rep.Error = CheckWeightRoundTrip(original.ID, original.WeightBP, got.WeightBP, tol)
		return rep
	}
	rep.OK = true
	return rep
}

func CheckWeightRoundTrip(strokeID string, originalBP, returnedBP, toleranceBP int) *model.Error {
	residual := absInt(returnedBP - originalBP)
	if residual <= toleranceBP {
		return nil
	}
	return &model.Error{
		Code: model.CodeRound, Field: "weight_bp", StrokeID: strokeID,
		Message:  "weight round-trip residual exceeds declared integer tolerance",
		Original: originalBP, Residual: residual,
		Details: map[string]any{"returned_bp": returnedBP, "tolerance_bp": toleranceBP},
	}
}

func absInt(v int) int {
	if v < 0 {
		return -v
	}
	return v
}

func hardnessSourceValue(present bool, value int) any {
	if !present {
		return nil
	}
	return value
}

func hardnessConversion(present bool, normal string) string {
	if !present {
		return "missing_hardness_use_declared_default_8000bp"
	}
	return normal
}
