package quantforensics

import "fmt"

func LoadProject(data []byte) (*Project, error) {
	root, err := decodeObject(data)
	if err != nil {
		return nil, err
	}
	if v, ok := root["schema"]; ok {
		s, ok := v.(string)
		if !ok {
			return nil, schemaError("$.schema", "schema version must be a string")
		}
		if s == SchemaV1 {
			return loadV1(root)
		}
		return nil, schemaError("$.schema", "unknown schema version %q; supported: %s,%s,%s", s, SchemaV1, SchemaV2, SchemaV3)
	}
	if v, ok := root["schema_version"]; ok {
		s, ok := v.(string)
		if !ok {
			return nil, schemaError("$.schema_version", "schema_version must be a string")
		}
		if s == SchemaV2 {
			return loadV2(root)
		}
		return nil, schemaError("$.schema_version", "unknown schema version %q", s)
	}
	if v, ok := root["schemaVersion"]; ok {
		s, ok := v.(string)
		if !ok {
			return nil, schemaError("$.schemaVersion", "schemaVersion must be a string")
		}
		if s == SchemaV3 {
			return loadV3(root)
		}
		return nil, schemaError("$.schemaVersion", "unknown schema version %q", s)
	}
	return nil, schemaError("$.schema", "missing schema version; expected one of schema, schema_version, schemaVersion")
}

func loadFrames(root map[string]any, key, idKey, widthKey, heightKey, dataKey string) ([]Image, map[string]Image, error) {
	arr, err := requireArray(root, key, "$")
	if err != nil {
		return nil, nil, err
	}
	images := make([]Image, 0, len(arr))
	byID := map[string]Image{}
	for i, item := range arr {
		p := arrayPath("$."+key, i)
		m, err := asObject(item, p)
		if err != nil {
			return nil, nil, err
		}
		id, err := requireString(m, idKey, p)
		if err != nil {
			return nil, nil, err
		}
		w64, wp, err := requireInt(m, widthKey, p)
		if err != nil {
			return nil, nil, err
		}
		h64, hp, err := requireInt(m, heightKey, p)
		if err != nil {
			return nil, nil, err
		}
		data, err := requireString(m, dataKey, p)
		if err != nil {
			return nil, nil, err
		}
		if w64 <= 0 || h64 <= 0 || w64 > 100000 || h64 > 100000 {
			return nil, nil, schemaError(wp, "frame dimensions out of supported range: %dx%d", w64, h64)
		}
		pixels, hash, err := decodeFramePixels(data, int(w64), int(h64), p+"."+dataKey)
		if err != nil {
			return nil, nil, err
		}
		im := Image{ID: id, Width: int(w64), Height: int(h64), Pixels: pixels, SHA256: hash}
		images = append(images, im)
		byID[id] = im
		_ = hp
	}
	return images, byID, nil
}

func loadTolerance(m map[string]any, key, parent, style string) (Tolerance, error) {
	obj, err := requireObject(m, key, parent)
	if err != nil {
		return Tolerance{}, err
	}
	compKey, weightKey := "maxComponent", "maxWeighted"
	switch style {
	case "v2":
		compKey, weightKey = "max_component_deviation", "max_weighted_color_diff"
	case "v3":
		compKey, weightKey = "maxComponentDeviation", "maxWeightedColorDiff"
	}
	comp, _, err := requireInt(obj, compKey, parent+"."+key)
	if err != nil {
		return Tolerance{}, err
	}
	weight, _, err := requireInt(obj, weightKey, parent+"."+key)
	if err != nil {
		return Tolerance{}, err
	}
	if comp < 0 || weight < 0 {
		return Tolerance{}, schemaError(parent+"."+key, "tolerances must be non-negative")
	}
	return Tolerance{MaxComponentDeviation: int(comp), MaxWeightedColorDiff: weight}, nil
}

func traceRename(traces *[]Trace, field, from, to, rule string, old, new any) {
	*traces = append(*traces, Trace{Field: field, FromPath: from, ToPath: to, Rule: rule, OldValue: old, NewValue: new})
}

func loadV1(root map[string]any) (*Project, error) {
	var traces []Trace
	id, err := requireString(root, "projectId", "$")
	if err != nil {
		return nil, err
	}
	seed, _, err := requireInt(root, "seed", "$")
	if err != nil {
		return nil, err
	}
	originX64, _, err := requireInt(root, "originX", "$")
	if err != nil {
		return nil, err
	}
	originY64, _, err := requireInt(root, "originY", "$")
	if err != nil {
		return nil, err
	}
	tileW64, _, err := requireInt(root, "tileW", "$")
	if err != nil {
		return nil, err
	}
	tileH64, _, err := requireInt(root, "tileH", "$")
	if err != nil {
		return nil, err
	}
	quantPermille, qp, err := requireInt(root, "quantPermille", "$")
	if err != nil {
		return nil, err
	}
	quantPPM, residual, err := ScaleToPPM(quantPermille, ScalePermille, qp)
	if err != nil {
		return nil, err
	}
	traces = append(traces, Trace{Field: "$.params.quantScalePPM", FromPath: qp, ToPath: "$.params.quantScalePPM", Rule: "permille_to_ppm_x1000", OldValue: quantPermille, NewValue: quantPPM, Residual: &residual})

	gainPPM := int64(1_000_000)
	if v, present, path, err := optionalIntDefault(root, "gainPermille", "$", 1000); err != nil {
		return nil, err
	} else if !present {
		traces = append(traces, Trace{Field: "$.params.gainPPM", FromPath: "$.gainPermille", ToPath: "$.params.gainPPM", Rule: "declared_default_gain_permille_1000", SourceDefault: true, DefaultValue: 1000, OldValue: "<missing>", NewValue: gainPPM})
	} else {
		gainPPM, residual, err = ScaleToPPM(v, ScalePermille, path)
		if err != nil {
			return nil, err
		}
		traces = append(traces, Trace{Field: "$.params.gainPPM", FromPath: path, Rule: "permille_to_ppm_x1000", OldValue: v, NewValue: gainPPM, Residual: &residual})
	}

	traces = append(traces, Trace{Field: "$.params.globalDitherPPM", FromPath: "$.ditherPermille", Rule: "v1_has_no_dither_field_declared_default_0", SourceDefault: true, DefaultValue: 0, OldValue: "<missing>", NewValue: 0})

	tol, err := loadTolerance(root, "tolerance", "$", "v1")
	if err != nil {
		return nil, err
	}
	metric := Metric(optionalString(root, "metric"))
	if metric == "" {
		metric = MetricWeightedRGB
	}

	palObj, err := requireObject(root, "palette", "$")
	if err != nil {
		return nil, err
	}
	constants, err := requireArray(palObj, "constants", "$.palette")
	if err != nil {
		return nil, err
	}
	entries := make([]PaletteEntry, 0, len(constants))
	for i, item := range constants {
		p := arrayPath("$.palette.constants", i)
		m, err := asObject(item, p)
		if err != nil {
			return nil, err
		}
		idx64, _, err := requireInt(m, "index", p)
		if err != nil {
			return nil, err
		}
		eid, err := requireString(m, "id", p)
		if err != nil {
			return nil, err
		}
		label := optionalString(m, "label")
		if label == "" {
			label = eid
		}
		rgbString, err := requireString(m, "rgb", p)
		if err != nil {
			return nil, err
		}
		color, err := parseHexColor(rgbString, p+".rgb")
		if err != nil {
			return nil, err
		}
		entryGain := int64(1_000_000)
		if g, present, gp, err := optionalIntDefault(m, "gainPermille", p, 1000); err != nil {
			return nil, err
		} else if !present {
			traces = append(traces, Trace{Field: fmt.Sprintf("$.palette.entries[%d].gainPPM", i), FromPath: gp, Rule: "declared_default_entry_gain_1000", SourceDefault: true, DefaultValue: 1000, OldValue: "<missing>", NewValue: entryGain})
		} else {
			entryGain, residual, err = ScaleToPPM(g, ScalePermille, gp)
			if err != nil {
				return nil, err
			}
			traces = append(traces, Trace{Field: fmt.Sprintf("$.palette.entries[%d].gainPPM", i), FromPath: gp, Rule: "permille_to_ppm_x1000", OldValue: g, NewValue: entryGain, Residual: &residual})
		}
		entries = append(entries, PaletteEntry{Index: int(idx64), ID: eid, Name: label, Group: optionalString(m, "family"), Color: color, GainPPM: entryGain})
	}
	traces = append(traces, Trace{Field: "$.palette.organization", FromPath: "$.palette.constants", Rule: "flat_constant_table_to_internal_flat", NewValue: "flat"})

	images, imageByID, err := loadFrames(root, "frames", "frameId", "width", "height", "rgbBase64")
	if err != nil {
		return nil, err
	}
	blocksArr, err := requireArray(root, "blocks", "$")
	if err != nil {
		return nil, err
	}
	blocks, err := loadV1Blocks(blocksArr, originX64, originY64, tileW64, tileH64, imageByID, &traces)
	if err != nil {
		return nil, err
	}

	p := &Project{
		Schema: InternalSchema, SourceSchema: SchemaV1, ProjectID: id, Seed: seed,
		OriginX: int(originX64), OriginY: int(originY64), TileW: int(tileW64), TileH: int(tileH64),
		Images: images, Blocks: blocks,
		Palette:   Palette{Version: optionalString(palObj, "version"), Organization: "flat", Entries: entries},
		Params:    Params{Metric: metric, QuantScalePPM: quantPPM, GainPPM: gainPPM, GlobalDitherPPM: 0, ParamVersion: ParamFormatVersion, AlgoVersion: AlgoVersion, CoordDeriveVersion: CoordDeriveVersion},
		Tolerance: tol, Traces: traces,
	}
	if p.Palette.Version == "" {
		p.Palette.Version = "v1-flat"
	}
	return p, nil
}

func loadV1Blocks(arr []any, originX, originY, tileW, tileH int64, images map[string]Image, traces *[]Trace) ([]Block, error) {
	blocks := make([]Block, 0, len(arr))
	for i, item := range arr {
		p := arrayPath("$.blocks", i)
		m, err := asObject(item, p)
		if err != nil {
			return nil, err
		}
		id, err := requireString(m, "blockId", p)
		if err != nil {
			return nil, err
		}
		frameID, err := requireString(m, "frameId", p)
		if err != nil {
			return nil, err
		}
		if _, ok := images[frameID]; !ok {
			return nil, schemaError(p+".frameId", "frame %q is not declared in frames", frameID)
		}
		ord64, _, err := requireInt(m, "ordinal", p)
		if err != nil {
			return nil, err
		}
		rect, err := requireObject(m, "rect", p)
		if err != nil {
			return nil, err
		}
		x64, xp, err := requireInt(rect, "left", p+".rect")
		if err != nil {
			return nil, err
		}
		y64, yp, err := requireInt(rect, "top", p+".rect")
		if err != nil {
			return nil, err
		}
		w64, _, err := requireInt(rect, "width", p+".rect")
		if err != nil {
			return nil, err
		}
		h64, _, err := requireInt(rect, "height", p+".rect")
		if err != nil {
			return nil, err
		}
		col, remX, resX, err := AbsoluteToBlockIndex(x64, originX, tileW, "x", xp)
		if err != nil {
			return nil, err
		}
		row, remY, resY, err := AbsoluteToBlockIndex(y64, originY, tileH, "y", yp)
		if err != nil {
			return nil, err
		}
		*traces = append(*traces,
			Trace{Field: fmt.Sprintf("$.blocks[%d].col", i), FromPath: xp, Rule: "absolute_left_to_block_index_floor", OldValue: x64, NewValue: col, Residual: &resX},
			Trace{Field: fmt.Sprintf("$.blocks[%d].row", i), FromPath: yp, Rule: "absolute_top_to_block_index_floor", OldValue: y64, NewValue: row, Residual: &resY},
		)
		kind := kindFromString(optionalString(m, "kind"))
		blocks = append(blocks, Block{ID: id, ImageID: frameID, Index: int(ord64), Col: int(col), Row: int(row), X: int(x64), Y: int(y64), W: int(w64), H: int(h64), ResidualX: int(remX), ResidualY: int(remY), Kind: kind})
	}
	return blocks, nil
}
