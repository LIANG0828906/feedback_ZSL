package maskqc

import (
	"bytes"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"image"
	"image/draw"
	"image/png"
	"math"
)

const DefaultHardnessPermille = 700

// LoadResult contains the unified model and field-level conversion trail.
type LoadResult struct {
	Project *Project          `json:"project"`
	Trail   []TransformRecord `json:"trail"`
}

// ExportResult is a versioned document plus optional loss report.
type ExportResult struct {
	Schema string            `json:"schema"`
	Data   []byte            `json:"-"`
	Trail  []TransformRecord `json:"trail"`
	Loss   *Error            `json:"loss,omitempty"`
}

type componentWire struct {
	Seq      int    `json:"seq"`
	Bounds   Rect   `json:"bounds"`
	AlphaB64 string `json:"alpha_base64"`
	Hash     string `json:"hash"`
}

type assetsWire struct {
	ImagePNGB64       string          `json:"image_png_base64,omitempty"`
	CandidateAlphaB64 string          `json:"candidate_alpha_base64"`
	TruthMaskB64      string          `json:"truth_mask_base64,omitempty"`
	TruthDomains      []DomainMetric  `json:"truth_domains,omitempty"`
	Components        []componentWire `json:"stroke_components,omitempty"`
}

// LoadProjectDocument parses any supported schema version.
func LoadProjectDocument(data []byte) (*LoadResult, error) {
	var doc map[string]any
	if err := json.Unmarshal(data, &doc); err != nil {
		return nil, schemaError("", "$", "$", "invalid JSON document: "+err.Error())
	}
	version, perr := documentSchema(doc)
	if perr != nil {
		return nil, perr
	}

	trail := []TransformRecord{}
	id, perr := requiredString(doc, "id", "/id", version)
	if perr != nil {
		return nil, perr
	}
	seed, perr := requiredInt64(doc, "seed", "/seed", version)
	if perr != nil {
		return nil, perr
	}
	width, perr := requiredInt(doc, "width", "/width", version)
	if perr != nil {
		return nil, perr
	}
	height, perr := requiredInt(doc, "height", "/height", version)
	if perr != nil {
		return nil, perr
	}
	if width <= 0 || height <= 0 {
		e := schemaError(version, "/width", "width", "width and height must be positive")
		e.Details = map[string]any{"width": width, "height": height}
		return nil, e
	}

	p := &Project{ID: id, Seed: seed, Width: width, Height: height, SourceVersion: version}

	switch version {
	case SchemaV1:
		strokes, records, err := loadV1Strokes(doc, version, width)
		if err != nil {
			return nil, err
		}
		p.Strokes = strokes
		trail = append(trail, records...)
		trail = append(trail, TransformRecord{Path: "/passes", Field: "passes", Op: "semantic_mapping", SourceVersion: version, Old: "stroke_sequence", New: "Strokes", Reason: "v1 passes is the construction stroke list"})
	case SchemaV2:
		channels, records, err := loadV2Channels(doc, version)
		if err != nil {
			return nil, err
		}
		p.ChannelCoverage = channels
		trail = append(trail, records...)
		strokes, records, err := loadV2Strokes(doc, version)
		if err != nil {
			return nil, err
		}
		p.Strokes = strokes
		trail = append(trail, records...)
		trail = append(trail,
			TransformRecord{Path: "/passes", Field: "passes", Op: "semantic_mapping", SourceVersion: version, Old: "channel_coverage_sequence", New: "ChannelCoverage", Reason: "v2 passes reuses the v1 stroke-list field for channel coverage"},
			TransformRecord{Path: "/channels", Field: "channels", Op: "semantic_mapping", SourceVersion: version, Old: "stroke_sequence", New: "Strokes", Reason: "v2 channels carries construction strokes after the list-field semantic swap"},
		)
	case SchemaV3:
		strokes, records, err := loadV3Strokes(doc, version)
		if err != nil {
			return nil, err
		}
		p.Strokes = strokes
		trail = append(trail, records...)
		channels, err := optionalIntSlice(doc, "channel_coverage", "/channel_coverage", version)
		if err != nil {
			return nil, err
		}
		p.ChannelCoverage = channels
	}

	if assetsValue, ok := doc["assets"]; ok {
		assetsMap, ok := assetsValue.(map[string]any)
		if !ok {
			return nil, schemaError(version, "/assets", "assets", "assets must be an object")
		}
		if err := decodeAssets(assetsMap, p, version); err != nil {
			return nil, err
		}
	}

	p.Trail = trail
	return &LoadResult{Project: p, Trail: trail}, nil
}

func documentSchema(doc map[string]any) (string, *Error) {
	raw, ok := doc["schema"]
	if !ok {
		return "", schemaError("", "/schema", "schema", "missing required field schema")
	}
	version, ok := raw.(string)
	if !ok || version == "" {
		return "", schemaError("", "/schema", "schema", "schema must be a non-empty string")
	}
	switch version {
	case SchemaV1, SchemaV2, SchemaV3:
		return version, nil
	default:
		return "", schemaError(version, "/schema", "schema", "unknown schema version")
	}
}

func loadV1Strokes(doc map[string]any, version string, width int) ([]Stroke, []TransformRecord, *Error) {
	items, err := requiredSlice(doc, "passes", "/passes", version)
	if err != nil {
		return nil, nil, err
	}
	strokes := make([]Stroke, 0, len(items))
	trail := []TransformRecord{}
	for i, raw := range items {
		path := fmt.Sprintf("/passes/%d", i)
		m, perr := asMap(raw, path, version)
		if perr != nil {
			return nil, nil, perr
		}
		seq, perr := requiredInt(m, "id", path+"/id", version)
		if perr != nil {
			return nil, nil, perr
		}
		priority, perr := requiredInt(m, "ord", path+"/ord", version)
		if perr != nil {
			return nil, nil, perr
		}
		fromRaw, perr := requiredAny(m, "from", path+"/from", version)
		if perr != nil {
			return nil, nil, perr
		}
		toRaw, perr := requiredAny(m, "to", path+"/to", version)
		if perr != nil {
			return nil, nil, perr
		}
		fromOffset, perr := pointFromValue(fromRaw, path+"/from", version)
		if perr != nil {
			return nil, nil, perr
		}
		toOffset, perr := pointFromValue(toRaw, path+"/to", version)
		if perr != nil {
			return nil, nil, perr
		}
		start, rec, cerr := OffsetToAbsolute(fromOffset, seq, width, path+"/from")
		if cerr != nil {
			cerr.SchemaVersion = version
			return nil, nil, cerr
		}
		rec.SourceVersion = version
		trail = append(trail, rec)
		end, rec, cerr := OffsetToAbsolute(toOffset, seq, width, path+"/to")
		if cerr != nil {
			cerr.SchemaVersion = version
			return nil, nil, cerr
		}
		rec.SourceVersion = version
		trail = append(trail, rec)

		level, perr := requiredInt(m, "level", path+"/level", version)
		if perr != nil {
			return nil, nil, perr
		}
		if level < 0 || level > WeightMaxV1 {
			e := paramError(path+"/level", 0, WeightMaxV1, "v1 coverage weight scale out of range")
			e.SchemaVersion = version
			e.Path = path + "/level"
			return nil, nil, e
		}
		rem, _ := optionalInt64(m, "level_rem", path+"/level_rem", version)
		weight, rerr := RestoreScale(level, rem, WeightMaxInternal, WeightMaxV1, path+"/level")
		if rerr != nil {
			rerr.SchemaVersion = version
			rerr.Path = path + "/level"
			return nil, nil, rerr
		}
		span, perr := requiredInt(m, "span", path+"/span", version)
		if perr != nil {
			return nil, nil, perr
		}
		if span < MinRadiusTick || span > MaxRadiusTick {
			e := paramError(path+"/span", MinRadiusTick, MaxRadiusTick, "radius tick out of range")
			e.SchemaVersion = version
			e.Path = path + "/span"
			return nil, nil, e
		}
		digest, _ := optionalString(m, "digest")
		s := Stroke{
			Seq: seq, OrderPriority: priority, Start: start, End: end,
			WeightPermille: weight, RadiusTick: span,
			HardnessPermille: DefaultHardnessPermille,
			WeightRemainder:  rem, ResidualDigest: digest,
		}
		if verr := ValidateStrokeParameters(s); verr != nil {
			verr.SchemaVersion = version
			return nil, nil, verr
		}
		strokes = append(strokes, s)
		trail = append(trail,
			TransformRecord{Path: path + "/level", Field: "weight_permille", Op: "scale_restore", SourceVersion: version, Old: level, New: weight, Rounding: "floor_with_retained_remainder", Remainder: rem},
			TransformRecord{Path: path + "/level", Field: "weight_permille", Op: "semantic_mapping", SourceVersion: version, Old: "coverage_weight_0_100", New: "WeightPermille"},
			TransformRecord{Path: path + "/span", Field: "radius_tick", Op: "semantic_mapping", SourceVersion: version, Old: "radius_tick", New: "RadiusTick"},
			TransformRecord{Path: path + "/hardness", Field: "hardness_permille", Op: "default", SourceVersion: version, Old: nil, New: DefaultHardnessPermille, DefaultValue: DefaultHardnessPermille, Reason: "hardness exists only in " + SchemaV3},
		)
	}
	return strokes, trail, nil
}

func loadV2Channels(doc map[string]any, version string) ([]int, []TransformRecord, *Error) {
	raw, perr := requiredSlice(doc, "passes", "/passes", version)
	if perr != nil {
		return nil, nil, perr
	}
	out := make([]int, 0, len(raw))
	for i, item := range raw {
		path := fmt.Sprintf("/passes/%d", i)
		v, ok := item.(float64)
		if !ok || math.Trunc(v) != v || v < 0 || v > WeightMaxV2 {
			e := paramError(path, 0, WeightMaxV2, "v2 channel coverage must be integer 0..255")
			e.SchemaVersion = version
			e.Path = path
			return nil, nil, e
		}
		out = append(out, int(math.Round(v*1000/255.0)))
	}
	return out, []TransformRecord{{Path: "/passes", Field: "channel_coverage", Op: "scale_restore", SourceVersion: version, Old: "0..255", New: "permille rounded"}}, nil
}

func loadV2Strokes(doc map[string]any, version string) ([]Stroke, []TransformRecord, *Error) {
	items, err := requiredSlice(doc, "channels", "/channels", version)
	if err != nil {
		return nil, nil, err
	}
	strokes := make([]Stroke, 0, len(items))
	trail := []TransformRecord{}
	for i, raw := range items {
		path := fmt.Sprintf("/channels/%d", i)
		m, perr := asMap(raw, path, version)
		if perr != nil {
			return nil, nil, perr
		}
		seq, perr := requiredInt(m, "no", path+"/no", version)
		if perr != nil {
			return nil, nil, perr
		}
		priority, perr := requiredInt(m, "rank", path+"/rank", version)
		if perr != nil {
			return nil, nil, perr
		}
		aRaw, perr := requiredAny(m, "a", path+"/a", version)
		if perr != nil {
			return nil, nil, perr
		}
		bRaw, perr := requiredAny(m, "b", path+"/b", version)
		if perr != nil {
			return nil, nil, perr
		}
		start, perr := pointFromValue(aRaw, path+"/a", version)
		if perr != nil {
			return nil, nil, perr
		}
		end, perr := pointFromValue(bRaw, path+"/b", version)
		if perr != nil {
			return nil, nil, perr
		}
		level, perr := requiredInt(m, "level", path+"/level", version)
		if perr != nil {
			return nil, nil, perr
		}
		if level < MinRadiusTick || level > MaxRadiusTick {
			e := paramError(path+"/level", MinRadiusTick, MaxRadiusTick, "in v2 level semantically means radius tick")
			e.SchemaVersion = version
			e.Path = path + "/level"
			return nil, nil, e
		}
		span, perr := requiredInt(m, "span", path+"/span", version)
		if perr != nil {
			return nil, nil, perr
		}
		if span < 0 || span > WeightMaxV2 {
			e := paramError(path+"/span", 0, WeightMaxV2, "in v2 span semantically means coverage weight 0..255")
			e.SchemaVersion = version
			e.Path = path + "/span"
			return nil, nil, e
		}
		rem, _ := optionalInt64(m, "span_rem", path+"/span_rem", version)
		weight, rerr := RestoreScale(span, rem, WeightMaxInternal, WeightMaxV2, path+"/span")
		if rerr != nil {
			rerr.SchemaVersion = version
			rerr.Path = path + "/span"
			return nil, nil, rerr
		}
		digest, _ := optionalString(m, "digest")
		s := Stroke{
			Seq: seq, OrderPriority: priority, Start: start, End: end,
			WeightPermille: weight, RadiusTick: level,
			HardnessPermille: DefaultHardnessPermille,
			WeightRemainder:  rem, ResidualDigest: digest,
		}
		if verr := ValidateStrokeParameters(s); verr != nil {
			verr.SchemaVersion = version
			return nil, nil, verr
		}
		strokes = append(strokes, s)
		trail = append(trail,
			TransformRecord{Path: path + "/level", Field: "radius_tick", Op: "semantic_swap", SourceVersion: version, Old: "v1 coverage_weight_field", New: "v2 radius_tick"},
			TransformRecord{Path: path + "/span", Field: "weight_permille", Op: "semantic_swap_scale_restore", SourceVersion: version, Old: span, New: weight, Rounding: "floor_with_retained_remainder", Remainder: rem},
			TransformRecord{Path: path + "/hardness", Field: "hardness_permille", Op: "default", SourceVersion: version, Old: nil, New: DefaultHardnessPermille, DefaultValue: DefaultHardnessPermille},
		)
	}
	return strokes, trail, nil
}

func loadV3Strokes(doc map[string]any, version string) ([]Stroke, []TransformRecord, *Error) {
	items, err := requiredSlice(doc, "strokes", "/strokes", version)
	if err != nil {
		return nil, nil, err
	}
	strokes := make([]Stroke, 0, len(items))
	for i, raw := range items {
		path := fmt.Sprintf("/strokes/%d", i)
		m, perr := asMap(raw, path, version)
		if perr != nil {
			return nil, nil, perr
		}
		seq, perr := requiredInt(m, "seq", path+"/seq", version)
		if perr != nil {
			return nil, nil, perr
		}
		priority, perr := requiredInt(m, "priority", path+"/priority", version)
		if perr != nil {
			return nil, nil, perr
		}
		startRaw, perr := requiredAny(m, "start", path+"/start", version)
		if perr != nil {
			return nil, nil, perr
		}
		endRaw, perr := requiredAny(m, "end", path+"/end", version)
		if perr != nil {
			return nil, nil, perr
		}
		start, perr := pointFromValue(startRaw, path+"/start", version)
		if perr != nil {
			return nil, nil, perr
		}
		end, perr := pointFromValue(endRaw, path+"/end", version)
		if perr != nil {
			return nil, nil, perr
		}
		weight, perr := requiredInt(m, "weight_permille", path+"/weight_permille", version)
		if perr != nil {
			return nil, nil, perr
		}
		radius, perr := requiredInt(m, "radius_tick", path+"/radius_tick", version)
		if perr != nil {
			return nil, nil, perr
		}
		hardness, perr := requiredInt(m, "hardness_permille", path+"/hardness_permille", version)
		if perr != nil {
			return nil, nil, perr
		}
		rem, _ := optionalInt64(m, "weight_remainder", path+"/weight_remainder", version)
		digest, _ := optionalString(m, "residual_digest")
		s := Stroke{Seq: seq, OrderPriority: priority, Start: start, End: end, WeightPermille: weight, RadiusTick: radius, HardnessPermille: hardness, WeightRemainder: rem, ResidualDigest: digest}
		if verr := ValidateStrokeParameters(s); verr != nil {
			verr.SchemaVersion = version
			verr.Path = path
			return nil, nil, verr
		}
		strokes = append(strokes, s)
	}
	return strokes, []TransformRecord{{Path: "/strokes", Field: "strokes", Op: "direct_mapping", SourceVersion: version, Old: "stroke_sequence", New: "Strokes"}}, nil
}

// ExportProject converts the internal model to a specific schema document.
func ExportProject(p *Project, target string) (*ExportResult, error) {
	if p == nil {
		return nil, &Error{Code: CodeSchema, Path: "$", Message: "project is nil"}
	}
	switch target {
	case SchemaV1:
		return exportV1(p)
	case SchemaV2:
		return exportV2(p)
	case SchemaV3:
		return exportV3(p)
	default:
		return nil, schemaError(target, "/schema", "schema", "unknown export schema version")
	}
}

func exportV1(p *Project) (*ExportResult, error) {
	trail := []TransformRecord{}
	passes := make([]map[string]any, 0, len(p.Strokes))
	losses := []LossItem{}
	for i, s := range p.Strokes {
		base := fmt.Sprintf("/passes/%d", i)
		start, rec, err := AbsoluteToOffset(s.Start, s.Seq, p.Width, base+"/from")
		if err != nil {
			return nil, err
		}
		rec.TargetVersion = SchemaV1
		trail = append(trail, rec)
		end, rec, err := AbsoluteToOffset(s.End, s.Seq, p.Width, base+"/to")
		if err != nil {
			return nil, err
		}
		rec.TargetVersion = SchemaV1
		trail = append(trail, rec)
		level, rem, cerr := ConvertScale(s.WeightPermille, WeightMaxInternal, WeightMaxV1, base+"/level")
		if cerr != nil {
			return nil, cerr
		}
		digest := ResidualDigest(s.Seq, "weight->v1", rem)
		pass := map[string]any{
			"id": s.Seq, "ord": s.OrderPriority,
			"from": []int{start.X, start.Y}, "to": []int{end.X, end.Y},
			"level": level, "span": s.RadiusTick,
			"level_rem": rem, "digest": digest,
		}
		passes = append(passes, pass)
		trail = append(trail,
			TransformRecord{Path: base + "/level", Field: "weight_permille", Op: "scale_convert", TargetVersion: SchemaV1, Old: s.WeightPermille, New: level, Rounding: "floor", Remainder: rem, TruncatedAt: "product/1000"},
			TransformRecord{Path: base + "/level", Field: "weight_permille", Op: "semantic_mapping", TargetVersion: SchemaV1, Old: "WeightPermille", New: "coverage_weight_0_100"},
		)
		losses = append(losses, LossItem{Path: base + "/hardness", Field: "hardness_permille", Disposition: "omit_and_use_default_700_on_reimport", Old: s.HardnessPermille, Reason: "v1 has no brush hardness field"})
	}
	losses = append(losses, LossItem{Path: "/channel_coverage", Field: "channel_coverage", Disposition: "omit_list", Old: p.ChannelCoverage, Reason: "v1 has no channel coverage sequence"})
	doc := map[string]any{
		"schema": SchemaV1, "id": p.ID, "seed": p.Seed, "width": p.Width, "height": p.Height,
		"passes": passes, "assets": encodeAssets(p),
	}
	data, _ := json.MarshalIndent(doc, "", "  ")
	res := &ExportResult{Schema: SchemaV1, Data: data, Trail: trail}
	res.Loss = lossReport(losses)
	return res, res.Loss
}

func exportV2(p *Project) (*ExportResult, error) {
	trail := []TransformRecord{}
	strokes := make([]map[string]any, 0, len(p.Strokes))
	losses := []LossItem{}
	for i, s := range p.Strokes {
		base := fmt.Sprintf("/channels/%d", i)
		span, rem, cerr := ConvertScale(s.WeightPermille, WeightMaxInternal, WeightMaxV2, base+"/span")
		if cerr != nil {
			return nil, cerr
		}
		digest := ResidualDigest(s.Seq, "weight->v2", rem)
		strokes = append(strokes, map[string]any{
			"no": s.Seq, "rank": s.OrderPriority,
			"a": []int{s.Start.X, s.Start.Y}, "b": []int{s.End.X, s.End.Y},
			"level": s.RadiusTick, "span": span, "span_rem": rem, "digest": digest,
		})
		trail = append(trail,
			TransformRecord{Path: base + "/level", Field: "radius_tick", Op: "semantic_swap", TargetVersion: SchemaV2, Old: "v1 weight_field_position", New: "radius_tick"},
			TransformRecord{Path: base + "/span", Field: "weight_permille", Op: "scale_convert", TargetVersion: SchemaV2, Old: s.WeightPermille, New: span, Rounding: "floor", Remainder: rem},
		)
		losses = append(losses, LossItem{Path: base + "/hardness", Field: "hardness_permille", Disposition: "omit_and_use_default_700_on_reimport", Old: s.HardnessPermille, Reason: "hardness exists only in v3"})
	}
	passes := make([]int, 0, len(p.ChannelCoverage))
	for _, c := range p.ChannelCoverage {
		v, _, _ := ConvertScale(clampPermille(c), WeightMaxInternal, WeightMaxV2, "/passes")
		passes = append(passes, v)
	}
	doc := map[string]any{
		"schema": SchemaV2, "id": p.ID, "seed": p.Seed, "width": p.Width, "height": p.Height,
		"channels": strokes, "passes": passes, "assets": encodeAssets(p),
	}
	data, _ := json.MarshalIndent(doc, "", "  ")
	res := &ExportResult{Schema: SchemaV2, Data: data, Trail: trail}
	res.Loss = lossReport(losses)
	return res, res.Loss
}

func exportV3(p *Project) (*ExportResult, error) {
	strokes := make([]map[string]any, 0, len(p.Strokes))
	for _, s := range p.Strokes {
		strokes = append(strokes, map[string]any{
			"seq": s.Seq, "priority": s.OrderPriority,
			"start":           map[string]int{"x": s.Start.X, "y": s.Start.Y},
			"end":             map[string]int{"x": s.End.X, "y": s.End.Y},
			"weight_permille": s.WeightPermille, "radius_tick": s.RadiusTick,
			"hardness_permille": s.HardnessPermille,
			"weight_remainder":  s.WeightRemainder, "residual_digest": s.ResidualDigest,
		})
	}
	doc := map[string]any{
		"schema": SchemaV3, "id": p.ID, "seed": p.Seed, "width": p.Width, "height": p.Height,
		"strokes": strokes, "channel_coverage": p.ChannelCoverage, "assets": encodeAssets(p),
	}
	data, _ := json.MarshalIndent(doc, "", "  ")
	return &ExportResult{Schema: SchemaV3, Data: data}, nil
}

// LossItem records one non-representable field and its disposition.
type LossItem struct {
	Path        string `json:"path"`
	Field       string `json:"field"`
	Disposition string `json:"disposition"`
	Old         any    `json:"old,omitempty"`
	Reason      string `json:"reason"`
}

func lossReport(items []LossItem) *Error {
	return &Error{Code: CodeLossy, Message: fmt.Sprintf("%d field(s) are lossy in target schema", len(items)), Details: map[string]any{"items": items}}
}

func encodeAssets(p *Project) assetsWire {
	a := assetsWire{}
	if p.Image != nil {
		var buf bytes.Buffer
		if err := png.Encode(&buf, p.Image); err == nil {
			a.ImagePNGB64 = base64.StdEncoding.EncodeToString(buf.Bytes())
		}
	}
	if p.Candidate != nil {
		a.CandidateAlphaB64 = base64.StdEncoding.EncodeToString(p.Candidate.Pix)
	}
	if p.Truth != nil {
		a.TruthDomains = p.Truth.Domains
		if p.Truth.Mask != nil {
			a.TruthMaskB64 = base64.StdEncoding.EncodeToString(p.Truth.Mask.Pix)
		}
		seqs := make([]int, 0, len(p.Truth.StrokeComponents))
		for seq := range p.Truth.StrokeComponents {
			seqs = append(seqs, seq)
		}
		// Deterministic component ordering by stroke sequence.
		for _, seq := range sortedInts(seqs) {
			c := p.Truth.StrokeComponents[seq]
			a.Components = append(a.Components, componentWire{Seq: c.StrokeSeq, Bounds: c.Bounds, AlphaB64: base64.StdEncoding.EncodeToString(c.Alpha), Hash: c.ContentHash})
		}
	}
	return a
}

func decodeAssets(m map[string]any, p *Project, version string) *Error {
	raw, err := json.Marshal(m)
	if err != nil {
		return schemaError(version, "/assets", "assets", err.Error())
	}
	var a assetsWire
	if err := json.Unmarshal(raw, &a); err != nil {
		return schemaError(version, "/assets", "assets", err.Error())
	}
	if a.ImagePNGB64 != "" {
		b, err := base64.StdEncoding.DecodeString(a.ImagePNGB64)
		if err != nil {
			return schemaError(version, "/assets/image_png_base64", "image", "invalid base64 PNG")
		}
		img, err := png.Decode(bytes.NewReader(b))
		if err != nil {
			return schemaError(version, "/assets/image_png_base64", "image", "invalid PNG")
		}
		rgba := image.NewRGBA(img.Bounds())
		draw.Draw(rgba, img.Bounds(), img, img.Bounds().Min, draw.Src)
		p.Image = rgba
	}
	if a.CandidateAlphaB64 != "" {
		b, err := base64.StdEncoding.DecodeString(a.CandidateAlphaB64)
		if err != nil {
			return schemaError(version, "/assets/candidate_alpha_base64", "candidate", "invalid base64 alpha")
		}
		if len(b) != p.Width*p.Height {
			return schemaError(version, "/assets/candidate_alpha_base64", "candidate", fmt.Sprintf("alpha length %d != %d", len(b), p.Width*p.Height))
		}
		p.Candidate = grayFromAlpha(b, p.Width, p.Height)
	}
	if a.TruthMaskB64 != "" || len(a.TruthDomains) > 0 || len(a.Components) > 0 {
		t := &Truth{Domains: a.TruthDomains, StrokeComponents: map[int]*CoverageComponent{}, ComponentHashes: map[int]string{}}
		if a.TruthMaskB64 != "" {
			b, err := base64.StdEncoding.DecodeString(a.TruthMaskB64)
			if err != nil {
				return schemaError(version, "/assets/truth_mask_base64", "truth", "invalid truth mask base64")
			}
			if len(b) != p.Width*p.Height {
				return schemaError(version, "/assets/truth_mask_base64", "truth", "truth mask length mismatch")
			}
			t.Mask = grayFromAlpha(b, p.Width, p.Height)
			t.ReferenceHash = hashBytes([]byte("truth-mask"), b)
		}
		for _, cw := range a.Components {
			b, err := base64.StdEncoding.DecodeString(cw.AlphaB64)
			if err != nil {
				return schemaError(version, "/assets/stroke_components", "components", "invalid component base64")
			}
			wantLen := cw.Bounds.Width() * cw.Bounds.Height()
			if len(b) != wantLen {
				return schemaError(version, "/assets/stroke_components", "components", fmt.Sprintf("component %d length mismatch", cw.Seq))
			}
			c := &CoverageComponent{StrokeSeq: cw.Seq, Bounds: cw.Bounds, Alpha: b, ContentHash: cw.Hash}
			if c.ContentHash == "" {
				c.ContentHash = hashBytes([]byte(fmt.Sprintf("stroke-%d-%s", cw.Seq, AlgorithmVersion)), b)
			}
			t.StrokeComponents[cw.Seq] = c
			t.ComponentHashes[cw.Seq] = c.ContentHash
		}
		p.Truth = t
	}
	return nil
}

func requiredString(m map[string]any, key, path, version string) (string, *Error) {
	v, ok := m[key]
	if !ok || v == nil {
		return "", schemaError(version, path, key, "missing required field")
	}
	s, ok := v.(string)
	if !ok || s == "" {
		return "", schemaError(version, path, key, "field must be a non-empty string")
	}
	return s, nil
}

func requiredAny(m map[string]any, key, path, version string) (any, *Error) {
	v, ok := m[key]
	if !ok || v == nil {
		return nil, schemaError(version, path, key, "missing required field")
	}
	return v, nil
}

func requiredSlice(m map[string]any, key, path, version string) ([]any, *Error) {
	v, perr := requiredAny(m, key, path, version)
	if perr != nil {
		return nil, perr
	}
	s, ok := v.([]any)
	if !ok {
		return nil, schemaError(version, path, key, "field must be a list")
	}
	return s, nil
}

func asMap(v any, path, version string) (map[string]any, *Error) {
	m, ok := v.(map[string]any)
	if !ok {
		return nil, schemaError(version, path, path, "field must be an object")
	}
	return m, nil
}

func requiredInt(m map[string]any, key, path, version string) (int, *Error) {
	v, perr := requiredAny(m, key, path, version)
	if perr != nil {
		return 0, perr
	}
	f, ok := v.(float64)
	if !ok || math.Trunc(f) != f || f < float64(math.MinInt) || f > float64(math.MaxInt) {
		return 0, schemaError(version, path, key, "field must be an integer")
	}
	return int(f), nil
}

func requiredInt64(m map[string]any, key, path, version string) (int64, *Error) {
	v, perr := requiredAny(m, key, path, version)
	if perr != nil {
		return 0, perr
	}
	f, ok := v.(float64)
	if !ok || math.Trunc(f) != f {
		return 0, schemaError(version, path, key, "field must be an integer")
	}
	return int64(f), nil
}

func optionalInt(m map[string]any, key, path, version string) (int, *Error) {
	if _, ok := m[key]; !ok {
		return 0, nil
	}
	return requiredInt(m, key, path, version)
}

func optionalInt64(m map[string]any, key, path, version string) (int64, *Error) {
	if _, ok := m[key]; !ok {
		return 0, nil
	}
	return requiredInt64(m, key, path, version)
}

func optionalString(m map[string]any, key string) (string, bool) {
	v, ok := m[key]
	if !ok {
		return "", false
	}
	s, ok := v.(string)
	return s, ok
}

func optionalIntSlice(m map[string]any, key, path, version string) ([]int, *Error) {
	raw, ok := m[key]
	if !ok {
		return nil, nil
	}
	items, ok := raw.([]any)
	if !ok {
		e := schemaError(version, path, key, "must be a list of integers")
		return nil, e
	}
	out := make([]int, 0, len(items))
	for i, item := range items {
		f, ok := item.(float64)
		if !ok || math.Trunc(f) != f {
			return nil, schemaError(version, fmt.Sprintf("%s/%d", path, i), key, "must be integer")
		}
		out = append(out, int(f))
	}
	return out, nil
}

func pointFromValue(v any, path, version string) (Point, *Error) {
	switch t := v.(type) {
	case map[string]any:
		x, err := requiredInt(t, "x", path+"/x", version)
		if err != nil {
			return Point{}, err
		}
		y, err := requiredInt(t, "y", path+"/y", version)
		if err != nil {
			return Point{}, err
		}
		return Point{X: x, Y: y}, nil
	case []any:
		if len(t) != 2 {
			return Point{}, schemaError(version, path, path, "coordinate must be [x,y]")
		}
		xa, ok := t[0].(float64)
		ya, ok2 := t[1].(float64)
		if !ok || !ok2 || math.Trunc(xa) != xa || math.Trunc(ya) != ya {
			return Point{}, schemaError(version, path, path, "coordinate entries must be integers")
		}
		return Point{X: int(xa), Y: int(ya)}, nil
	default:
		return Point{}, schemaError(version, path, path, "coordinate must be {x,y} or [x,y]")
	}
}

func clampPermille(v int) int {
	if v < 0 {
		return 0
	}
	if v > 1000 {
		return 1000
	}
	return v
}

func sortedInts(in []int) []int {
	out := append([]int(nil), in...)
	for i := 0; i < len(out); i++ {
		for j := i + 1; j < len(out); j++ {
			if out[j] < out[i] {
				out[i], out[j] = out[j], out[i]
			}
		}
	}
	return out
}
