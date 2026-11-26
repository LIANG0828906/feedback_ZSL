package quantforensics

import "fmt"

func loadV2(root map[string]any) (*Project, error) {
	var traces []Trace
	id, err := requireString(root, "project_id", "$")
	if err != nil {
		return nil, err
	}
	seed, _, err := requireInt(root, "seed", "$")
	if err != nil {
		return nil, err
	}
	originX, _, err := requireInt(root, "origin_x", "$")
	if err != nil {
		return nil, err
	}
	originY, _, err := requireInt(root, "origin_y", "$")
	if err != nil {
		return nil, err
	}
	tileW, _, err := requireInt(root, "tile_width", "$")
	if err != nil {
		return nil, err
	}
	tileH, _, err := requireInt(root, "tile_height", "$")
	if err != nil {
		return nil, err
	}
	quantV, quantPath, err := requireInt(root, "quant_scale_permyriad", "$")
	if err != nil {
		return nil, err
	}
	quantPPM, res, err := ScaleToPPM(quantV, ScalePermyriad, quantPath)
	if err != nil {
		return nil, err
	}
	traces = append(traces, Trace{Field: "$.params.quantScalePPM", FromPath: quantPath, Rule: "permyriad_to_ppm_x100", OldValue: quantV, NewValue: quantPPM, Residual: &res})

	gainPPM := int64(1_000_000)
	if gainV, present, path, err := optionalIntDefault(root, "gain_scale_permyriad", "$", 10000); err != nil {
		return nil, err
	} else if !present {
		traces = append(traces, defaultGainTrace("$.gain_scale_permyriad", "$.params.gainPPM", 10000))
	} else {
		gainPPM, res, err = ScaleToPPM(gainV, ScalePermyriad, path)
		if err != nil {
			return nil, err
		}
		traces = append(traces, Trace{Field: "$.params.gainPPM", FromPath: path, Rule: "permyriad_to_ppm_x100", OldValue: gainV, NewValue: gainPPM, Residual: &res})
	}

	ditherPPM := int64(0)
	if ditherV, present, path, err := optionalIntDefault(root, "dither_permyriad", "$", 0); err != nil {
		return nil, err
	} else if !present {
		traces = append(traces, Trace{Field: "$.params.globalDitherPPM", FromPath: path, Rule: "declared_default_dither_0", SourceDefault: true, DefaultValue: 0, OldValue: "<missing>", NewValue: 0})
	} else {
		ditherPPM, res, err = ScaleToPPM(ditherV, ScalePermyriad, path)
		if err != nil {
			return nil, err
		}
		traces = append(traces, Trace{Field: "$.params.globalDitherPPM", FromPath: path, Rule: "permyriad_to_ppm_x100", OldValue: ditherV, NewValue: ditherPPM, Residual: &res})
	}

	tol, err := loadTolerance(root, "tolerance", "$", "v2")
	if err != nil {
		return nil, err
	}
	metric := Metric(optionalString(root, "metric"))
	if metric == "" {
		metric = MetricWeightedRGB
	}

	rawEntries, err := requireArray(root, "palette_entries", "$")
	if err != nil {
		return nil, err
	}
	entries := make([]PaletteEntry, 0, len(rawEntries))
	for i, item := range rawEntries {
		p := arrayPath("$.palette_entries", i)
		m, err := asObject(item, p)
		if err != nil {
			return nil, err
		}
		idx, _, err := requireInt(m, "entry_index", p)
		if err != nil {
			return nil, err
		}
		eid, err := requireString(m, "entry_id", p)
		if err != nil {
			return nil, err
		}
		name := optionalString(m, "display_name")
		if name == "" {
			name = eid
		}
		hex, err := requireString(m, "color_hex", p)
		if err != nil {
			return nil, err
		}
		color, err := parseHexColor(hex, p+".color_hex")
		if err != nil {
			return nil, err
		}
		gain := int64(1_000_000)
		if g, present, gp, err := optionalIntDefault(m, "gain_permyriad", p, 10000); err != nil {
			return nil, err
		} else if !present {
			traces = append(traces, Trace{Field: fmt.Sprintf("$.palette.entries[%d].gainPPM", i), FromPath: gp, Rule: "declared_default_gain_10000_permyriad", SourceDefault: true, DefaultValue: 10000, OldValue: "<missing>", NewValue: gain})
		} else {
			gain, res, err = ScaleToPPM(g, ScalePermyriad, gp)
			if err != nil {
				return nil, err
			}
			traces = append(traces, Trace{Field: fmt.Sprintf("$.palette.entries[%d].gainPPM", i), FromPath: gp, Rule: "permyriad_to_ppm_x100", OldValue: g, NewValue: gain, Residual: &res})
		}
		entries = append(entries, PaletteEntry{Index: int(idx), ID: eid, Name: name, Group: optionalString(m, "group_name"), Color: color, GainPPM: gain})
	}
	traces = append(traces, Trace{Field: "$.palette.organization", FromPath: "$.palette_entries", Rule: "flat_palette_entries_to_internal_flat", NewValue: "flat"})

	images, imageByID, err := loadFrames(root, "frames", "image_id", "width", "height", "pixels_rgb_base64")
	if err != nil {
		return nil, err
	}
	rawBlocks, err := requireArray(root, "blocks", "$")
	if err != nil {
		return nil, err
	}
	blocks := make([]Block, 0, len(rawBlocks))
	for i, item := range rawBlocks {
		p := arrayPath("$.blocks", i)
		m, err := asObject(item, p)
		if err != nil {
			return nil, err
		}
		bid, err := requireString(m, "block_id", p)
		if err != nil {
			return nil, err
		}
		imageID, err := requireString(m, "image_id", p)
		if err != nil {
			return nil, err
		}
		if _, ok := imageByID[imageID]; !ok {
			return nil, schemaError(p+".image_id", "frame %q is not declared", imageID)
		}
		ord, _, err := requireInt(m, "ordinal", p)
		if err != nil {
			return nil, err
		}
		col64, cp, err := requireInt(m, "block_col", p)
		if err != nil {
			return nil, err
		}
		row64, rp, err := requireInt(m, "block_row", p)
		if err != nil {
			return nil, err
		}
		w64, _, err := requireInt(m, "block_width", p)
		if err != nil {
			return nil, err
		}
		h64, _, err := requireInt(m, "block_height", p)
		if err != nil {
			return nil, err
		}
		x, resX, err := BlockIndexToAbsolute(col64, originX, tileW, "x", cp)
		if err != nil {
			return nil, err
		}
		y, resY, err := BlockIndexToAbsolute(row64, originY, tileH, "y", rp)
		if err != nil {
			return nil, err
		}
		traces = append(traces,
			Trace{Field: fmt.Sprintf("$.blocks[%d].x", i), FromPath: cp, Rule: "block_col_to_absolute_origin_plus_col_times_width", OldValue: col64, NewValue: x, Residual: &resX},
			Trace{Field: fmt.Sprintf("$.blocks[%d].y", i), FromPath: rp, Rule: "block_row_to_absolute_origin_plus_row_times_height", OldValue: row64, NewValue: y, Residual: &resY},
		)
		blocks = append(blocks, Block{ID: bid, ImageID: imageID, Index: int(ord), Col: int(col64), Row: int(row64), X: int(x), Y: int(y), W: int(w64), H: int(h64), Kind: kindFromString(optionalString(m, "kind"))})
	}

	return &Project{
		Schema: InternalSchema, SourceSchema: SchemaV2, ProjectID: id, Seed: seed,
		OriginX: int(originX), OriginY: int(originY), TileW: int(tileW), TileH: int(tileH),
		Images: images, Blocks: blocks,
		Palette:   Palette{Version: optionalString(root, "palette_version"), Organization: "flat", Entries: entries},
		Params:    Params{Metric: metric, QuantScalePPM: quantPPM, GainPPM: gainPPM, GlobalDitherPPM: ditherPPM, ParamVersion: ParamFormatVersion, AlgoVersion: AlgoVersion, CoordDeriveVersion: CoordDeriveVersion},
		Tolerance: tol, Traces: traces,
	}, nil
}

func defaultGainTrace(from, to string, declaredDefault any) Trace {
	return Trace{Field: to, FromPath: from, Rule: "declared_default_gain", SourceDefault: true, DefaultValue: declaredDefault, OldValue: "<missing>", NewValue: int64(1_000_000)}
}
