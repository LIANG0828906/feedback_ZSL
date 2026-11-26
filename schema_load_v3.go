package quantforensics

import "fmt"

func loadV3(root map[string]any) (*Project, error) {
	var traces []Trace
	id, err := requireString(root, "projectID", "$")
	if err != nil {
		return nil, err
	}
	seed, _, err := requireInt(root, "seed", "$")
	if err != nil {
		return nil, err
	}
	grid, err := requireObject(root, "grid", "$")
	if err != nil {
		return nil, err
	}
	originX, _, err := requireInt(grid, "originX", "$.grid")
	if err != nil {
		return nil, err
	}
	originY, _, err := requireInt(grid, "originY", "$.grid")
	if err != nil {
		return nil, err
	}
	tileW, _, err := requireInt(grid, "tileWidth", "$.grid")
	if err != nil {
		return nil, err
	}
	tileH, _, err := requireInt(grid, "tileHeight", "$.grid")
	if err != nil {
		return nil, err
	}
	quantV, qp, err := requireInt(root, "quantScalePermyriad", "$")
	if err != nil {
		return nil, err
	}
	quantPPM, res, err := ScaleToPPM(quantV, ScalePermyriad, qp)
	if err != nil {
		return nil, err
	}
	traces = append(traces, Trace{Field: "$.params.quantScalePPM", FromPath: qp, Rule: "v3_permyriad_to_ppm_x100", OldValue: quantV, NewValue: quantPPM, Residual: &res})
	gainV, _, err := requireInt(root, "gainScalePermyriad", "$")
	if err != nil {
		return nil, err
	}
	gainPPM, res, err := ScaleToPPM(gainV, ScalePermyriad, "$.gainScalePermyriad")
	if err != nil {
		return nil, err
	}
	traces = append(traces, Trace{Field: "$.params.gainPPM", FromPath: "$.gainScalePermyriad", Rule: "v3_permyriad_to_ppm_x100", OldValue: gainV, NewValue: gainPPM, Residual: &res})

	globalDither := int64(0)
	ditherOverrides := map[string]int64{}
	if ditherObj, ok := root["dither"]; ok && ditherObj != nil {
		dm, err := asObject(ditherObj, "$.dither")
		if err != nil {
			return nil, err
		}
		if v, present, path, err := optionalIntDefault(dm, "globalPermyriad", "$.dither", 0); err != nil {
			return nil, err
		} else if present {
			globalDither, res, err = ScaleToPPM(v, ScalePermyriad, path)
			if err != nil {
				return nil, err
			}
			traces = append(traces, Trace{Field: "$.params.globalDitherPPM", FromPath: path, Rule: "v3_permyriad_to_ppm_x100", OldValue: v, NewValue: globalDither, Residual: &res})
		} else {
			traces = append(traces, Trace{Field: "$.params.globalDitherPPM", FromPath: "$.dither.globalPermyriad", Rule: "declared_default_dither_0", SourceDefault: true, DefaultValue: 0, OldValue: "<missing>", NewValue: 0})
		}
		if rawOverrides, ok := dm["perBlock"]; ok && rawOverrides != nil {
			arr, err := asArrayAny(rawOverrides, "$.dither.perBlock")
			if err != nil {
				return nil, err
			}
			for i, item := range arr {
				p := arrayPath("$.dither.perBlock", i)
				om, err := asObject(item, p)
				if err != nil {
					return nil, err
				}
				bid, err := requireString(om, "blockId", p)
				if err != nil {
					return nil, err
				}
				v, vp, err := requireInt(om, "permyriad", p)
				if err != nil {
					return nil, err
				}
				ppm, res, err := ScaleToPPM(v, ScalePermyriad, vp)
				if err != nil {
					return nil, err
				}
				ditherOverrides[bid] = ppm
				traces = append(traces, Trace{Field: "$.blocks." + bid + ".ditherPPM", FromPath: vp, Rule: "v3_block_dither_permyriad_to_ppm", OldValue: v, NewValue: ppm, Residual: &res})
			}
		}
	} else {
		traces = append(traces, Trace{Field: "$.params.globalDitherPPM", FromPath: "$.dither", Rule: "declared_default_dither_0", SourceDefault: true, DefaultValue: 0, OldValue: "<missing>", NewValue: 0})
	}

	tol, err := loadTolerance(root, "tolerance", "$", "v3")
	if err != nil {
		return nil, err
	}
	metric := Metric(optionalString(root, "metric"))
	if metric == "" {
		metric = MetricWeightedRGB
	}

	tree, err := requireObject(root, "paletteTree", "$")
	if err != nil {
		return nil, err
	}
	families, err := requireArray(tree, "families", "$.paletteTree")
	if err != nil {
		return nil, err
	}
	entries := []PaletteEntry{}
	paletteVersion := optionalString(tree, "version")
	for fi, familyItem := range families {
		fp := arrayPath("$.paletteTree.families", fi)
		family, err := asObject(familyItem, fp)
		if err != nil {
			return nil, err
		}
		groupID, err := requireString(family, "familyId", fp)
		if err != nil {
			return nil, err
		}
		groupName := optionalString(family, "familyName")
		if groupName == "" {
			groupName = groupID
		}
		swatches, err := requireArray(family, "swatches", fp)
		if err != nil {
			return nil, err
		}
		for si, swItem := range swatches {
			sp := arrayPath(fp+".swatches", si)
			sw, err := asObject(swItem, sp)
			if err != nil {
				return nil, err
			}
			idx, _, err := requireInt(sw, "index", sp)
			if err != nil {
				return nil, err
			}
			sid, err := requireString(sw, "swatchID", sp)
			if err != nil {
				return nil, err
			}
			label := optionalString(sw, "label")
			if label == "" {
				label = sid
			}
			hex, err := requireString(sw, "hex", sp)
			if err != nil {
				return nil, err
			}
			color, err := parseHexColor(hex, sp+".hex")
			if err != nil {
				return nil, err
			}
			gain := int64(1_000_000)
			if g, present, gp, err := optionalIntDefault(sw, "gainPermyriad", sp, 10000); err != nil {
				return nil, err
			} else if present {
				gain, res, err = ScaleToPPM(g, ScalePermyriad, gp)
				if err != nil {
					return nil, err
				}
				traces = append(traces, Trace{Field: fmt.Sprintf("$.palette.entries[%d].gainPPM", idx), FromPath: gp, Rule: "v3_permyriad_to_ppm_x100", OldValue: g, NewValue: gain, Residual: &res})
			}
			entries = append(entries, PaletteEntry{Index: int(idx), ID: sid, Name: label, Group: groupName, Color: color, GainPPM: gain})
		}
	}
	traces = append(traces, Trace{Field: "$.palette.organization", FromPath: "$.paletteTree.families", Rule: "grouped_color_family_tree_to_internal_grouped", NewValue: "grouped"})

	images, imageByID, err := loadFrames(root, "frames", "frameID", "width", "height", "rgbBase64")
	if err != nil {
		return nil, err
	}
	rawCells, err := requireArray(root, "cells", "$")
	if err != nil {
		return nil, err
	}
	blocks := make([]Block, 0, len(rawCells))
	for i, item := range rawCells {
		p := arrayPath("$.cells", i)
		m, err := asObject(item, p)
		if err != nil {
			return nil, err
		}
		bid, err := requireString(m, "cellID", p)
		if err != nil {
			return nil, err
		}
		frameID, err := requireString(m, "frameID", p)
		if err != nil {
			return nil, err
		}
		if _, ok := imageByID[frameID]; !ok {
			return nil, schemaError(p+".frameID", "frame %q is not declared", frameID)
		}
		ord, _, err := requireInt(m, "ordinal", p)
		if err != nil {
			return nil, err
		}
		col64, cp, err := requireInt(m, "col", p)
		if err != nil {
			return nil, err
		}
		row64, rp, err := requireInt(m, "row", p)
		if err != nil {
			return nil, err
		}
		w64, _, err := requireInt(m, "width", p)
		if err != nil {
			return nil, err
		}
		h64, _, err := requireInt(m, "height", p)
		if err != nil {
			return nil, err
		}
		x, rx, err := BlockIndexToAbsolute(col64, originX, tileW, "x", cp)
		if err != nil {
			return nil, err
		}
		y, ry, err := BlockIndexToAbsolute(row64, originY, tileH, "y", rp)
		if err != nil {
			return nil, err
		}
		block := Block{ID: bid, ImageID: frameID, Index: int(ord), Col: int(col64), Row: int(row64), X: int(x), Y: int(y), W: int(w64), H: int(h64), Kind: kindFromString(optionalString(m, "kind"))}
		if ppm, ok := ditherOverrides[bid]; ok {
			block.DitherPPM = ppm
			block.HasDitherOverride = true
		}
		traces = append(traces,
			Trace{Field: fmt.Sprintf("$.blocks[%d].x", i), FromPath: cp, Rule: "v3_col_to_absolute", OldValue: col64, NewValue: x, Residual: &rx},
			Trace{Field: fmt.Sprintf("$.blocks[%d].y", i), FromPath: rp, Rule: "v3_row_to_absolute", OldValue: row64, NewValue: y, Residual: &ry},
		)
		blocks = append(blocks, block)
	}
	for bid := range ditherOverrides {
		found := false
		for _, b := range blocks {
			if b.ID == bid {
				found = true
				break
			}
		}
		if !found {
			return nil, schemaError("$.dither.perBlock", "dither override references unknown block %q", bid)
		}
	}

	return &Project{
		Schema: InternalSchema, SourceSchema: SchemaV3, ProjectID: id, Seed: seed,
		OriginX: int(originX), OriginY: int(originY), TileW: int(tileW), TileH: int(tileH),
		Images: images, Blocks: blocks,
		Palette:   Palette{Version: paletteVersion, Organization: "grouped", Entries: entries},
		Params:    Params{Metric: metric, QuantScalePPM: quantPPM, GainPPM: gainPPM, GlobalDitherPPM: globalDither, ParamVersion: ParamFormatVersion, AlgoVersion: AlgoVersion, CoordDeriveVersion: CoordDeriveVersion},
		Tolerance: tol, Traces: traces,
	}, nil
}

func asArrayAny(v any, path string) ([]any, error) {
	a, ok := v.([]any)
	if !ok {
		return nil, schemaError(path, "required array has wrong type")
	}
	return a, nil
}
