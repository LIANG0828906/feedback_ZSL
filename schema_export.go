package quantforensics

import (
	"encoding/json"
	"fmt"
)

func cloneProject(p *Project) *Project {
	cp := *p
	cp.Images = append([]Image(nil), p.Images...)
	cp.Blocks = append([]Block(nil), p.Blocks...)
	cp.Palette.Entries = append([]PaletteEntry(nil), p.Palette.Entries...)
	cp.Traces = nil
	return &cp
}

func ExportProject(p *Project, target string) ([]byte, *ConversionReport, error) {
	degraded, report, err := DegradeProject(p, target)
	if err != nil {
		return nil, nil, err
	}
	var doc map[string]any
	switch target {
	case SchemaV1:
		doc = exportV1(degraded)
	case SchemaV2:
		doc = exportV2(degraded)
	case SchemaV3:
		doc = exportV3(degraded)
	default:
		return nil, nil, schemaError("$.targetSchema", "unknown export schema %q", target)
	}
	b, err := json.MarshalIndent(doc, "", "  ")
	if err != nil {
		return nil, report, err
	}
	report.OK = len(report.Losses) == 0
	return b, report, nil
}

func DegradeProject(p *Project, target string) (*Project, *ConversionReport, error) {
	cp := cloneProject(p)
	report := &ConversionReport{SourceVersion: p.SourceSchema, TargetVersion: target, OK: true}
	addLoss := func(item LossItem) {
		item.Code = string(CodeLossy)
		report.Losses = append(report.Losses, item)
	}

	quantizeScale := func(v int64, unit ScaleUnit, rounding, field string) (int64, error) {
		external, res, err := PPMToScale(v, unit, rounding, field)
		if err != nil {
			return 0, err
		}
		report.Traces = append(report.Traces, Trace{Field: field, Rule: "export_ppm_to_" + string(unit), OldValue: v, NewValue: external, Residual: &res})
		if res.Remainder != 0 {
			addLoss(LossItem{Field: field, Kind: "scale_truncation", Value: v, Reason: fmt.Sprintf("%s cannot represent PPM remainder %d", unit, res.Remainder), Disposition: "floor_to_declared_unit"})
			v -= res.Remainder
		}
		return v, nil
	}

	unit := ScalePermyriad
	if target == SchemaV1 {
		unit = ScalePermille
	}
	var err error
	cp.Params.QuantScalePPM, err = quantizeScale(cp.Params.QuantScalePPM, unit, "floor", "$.params.quantScalePPM")
	if err != nil {
		return nil, nil, err
	}
	cp.Params.GainPPM, err = quantizeScale(cp.Params.GainPPM, unit, "floor", "$.params.gainPPM")
	if err != nil {
		return nil, nil, err
	}
	for i := range cp.Palette.Entries {
		cp.Palette.Entries[i].GainPPM, err = quantizeScale(cp.Palette.Entries[i].GainPPM, unit, "floor", fmt.Sprintf("$.palette.entries[%d].gainPPM", i))
		if err != nil {
			return nil, nil, err
		}
	}

	if target == SchemaV2 || target == SchemaV3 {
		for i := range cp.Blocks {
			b := &cp.Blocks[i]
			col, remX, res, err := AbsoluteToBlockIndex(int64(b.X), int64(cp.OriginX), int64(cp.TileW), "x", fmt.Sprintf("$.blocks[%d].x", i))
			if err != nil {
				return nil, nil, err
			}
			row, remY, resY, err := AbsoluteToBlockIndex(int64(b.Y), int64(cp.OriginY), int64(cp.TileH), "y", fmt.Sprintf("$.blocks[%d].y", i))
			if err != nil {
				return nil, nil, err
			}
			b.Col, b.Row = int(col), int(row)
			b.ResidualX, b.ResidualY = int(remX), int(remY)
			if remX != 0 || remY != 0 {
				addLoss(LossItem{Field: fmt.Sprintf("$.blocks[%d].col", i), Kind: "coordinate_subtile_offset", Value: [2]int{int(remX), int(remY)}, Reason: "block-index coordinates discard unaligned pixel offset", Disposition: "snap_to_derived_absolute_origin"})
				b.X = cp.OriginX + int(col)*cp.TileW
				b.Y = cp.OriginY + int(row)*cp.TileH
			}
			report.Traces = append(report.Traces,
				Trace{Field: fmt.Sprintf("$.blocks[%d].col", i), Rule: "export_absolute_to_index", Residual: &res},
				Trace{Field: fmt.Sprintf("$.blocks[%d].row", i), Rule: "export_absolute_to_index", Residual: &resY},
			)
		}
	}

	if target == SchemaV1 || target == SchemaV2 {
		if cp.Palette.Organization == "grouped" {
			addLoss(LossItem{Field: "$.palette.organization", Kind: "grouped_palette_tree", Value: cp.Palette.Organization, Reason: target + " only declares a flat constant/entry table", Disposition: "flatten_groups_preserve_entry_values"})
			cp.Palette.Organization = "flat"
		}
	}

	switch target {
	case SchemaV2:
		for i := range cp.Blocks {
			b := &cp.Blocks[i]
			if b.HasDitherOverride {
				addLoss(LossItem{Field: fmt.Sprintf("$.blocks[%d].ditherPermyriad", i), Kind: "per_block_dither", Value: b.DitherPPM, Reason: SchemaV2 + " supports only global dither_permyriad", Disposition: "reset_block_to_global_dither"})
				b.HasDitherOverride = false
				b.DitherPPM = cp.Params.GlobalDitherPPM
			}
		}
	case SchemaV1:
		if cp.Params.GlobalDitherPPM != 0 {
			addLoss(LossItem{Field: "$.params.ditherPermille", Kind: "global_dither_field", Value: cp.Params.GlobalDitherPPM, Reason: SchemaV1 + " has no dither field", Disposition: "reset_to_declared_default_0"})
		}
		cp.Params.GlobalDitherPPM = 0
		for i := range cp.Blocks {
			b := &cp.Blocks[i]
			if b.HasDitherOverride || b.DitherPPM != 0 {
				addLoss(LossItem{Field: fmt.Sprintf("$.blocks[%d].ditherPermille", i), Kind: "per_block_dither", Value: b.DitherPPM, Reason: SchemaV1 + " has no dither field or per-block dither map", Disposition: "reset_to_declared_default_0"})
			}
			b.HasDitherOverride = false
			b.DitherPPM = 0
		}
		if cp.Params.GainPPM != 1_000_000 {
			addLoss(LossItem{Field: "$.gainPermille", Kind: "global_gain_field", Value: cp.Params.GainPPM, Reason: "exporter only writes non-default gain and loader default is 1000 permille", Disposition: "reset_to_default_gain"})
			cp.Params.GainPPM = 1_000_000
		}
	}
	return cp, report, nil
}

func exportV1(p *Project) map[string]any {
	const toUnit = ScalePermille
	q, _, _ := PPMToScale(p.Params.QuantScalePPM, toUnit, "floor", "quantPermille")
	doc := map[string]any{
		"schema":        SchemaV1,
		"projectId":     p.ProjectID,
		"seed":          p.Seed,
		"originX":       p.OriginX,
		"originY":       p.OriginY,
		"tileW":         p.TileW,
		"tileH":         p.TileH,
		"metric":        string(p.Params.Metric),
		"quantPermille": q,
		"tolerance":     map[string]any{"maxComponent": p.Tolerance.MaxComponentDeviation, "maxWeighted": p.Tolerance.MaxWeightedColorDiff},
		"palette": map[string]any{
			"version": p.Palette.Version,
			"mode":    "flat_constants",
			"constants": func() []any {
				out := make([]any, 0, len(p.Palette.Entries))
				for _, e := range p.Palette.Entries {
					g, _, _ := PPMToScale(e.GainPPM, toUnit, "floor", "gain")
					out = append(out, map[string]any{"index": e.Index, "id": e.ID, "label": e.Name, "family": e.Group, "rgb": hexColor(e.Color), "gainPermille": g})
				}
				return out
			}(),
		},
		"frames": func() []any {
			out := make([]any, 0, len(p.Images))
			for _, im := range p.Images {
				out = append(out, map[string]any{"frameId": im.ID, "width": im.Width, "height": im.Height, "rgbBase64": encodeFramePixels(im.Pixels), "sha256": im.SHA256})
			}
			return out
		}(),
		"blocks": func() []any {
			out := make([]any, 0, len(p.Blocks))
			for _, b := range p.Blocks {
				out = append(out, map[string]any{
					"blockId": b.ID, "frameId": b.ImageID, "ordinal": b.Index, "kind": string(b.Kind),
					"rect": map[string]any{"left": b.X, "top": b.Y, "width": b.W, "height": b.H},
				})
			}
			return out
		}(),
	}
	if p.Params.GainPPM != 1_000_000 {
		g, _, _ := PPMToScale(p.Params.GainPPM, toUnit, "floor", "gainPermille")
		doc["gainPermille"] = g
	}
	return doc
}

func exportV2(p *Project) map[string]any {
	q, _, _ := PPMToScale(p.Params.QuantScalePPM, ScalePermyriad, "floor", "quant")
	g, _, _ := PPMToScale(p.Params.GainPPM, ScalePermyriad, "floor", "gain")
	d, _, _ := PPMToScale(p.Params.GlobalDitherPPM, ScalePermyriad, "floor", "dither")
	return map[string]any{
		"schema_version":        SchemaV2,
		"palette_version":       p.Palette.Version,
		"project_id":            p.ProjectID,
		"seed":                  p.Seed,
		"origin_x":              p.OriginX,
		"origin_y":              p.OriginY,
		"tile_width":            p.TileW,
		"tile_height":           p.TileH,
		"metric":                string(p.Params.Metric),
		"quant_scale_permyriad": q,
		"gain_scale_permyriad":  g,
		"dither_permyriad":      d,
		"tolerance":             map[string]any{"max_component_deviation": p.Tolerance.MaxComponentDeviation, "max_weighted_color_diff": p.Tolerance.MaxWeightedColorDiff},
		"palette_entries": func() []any {
			out := make([]any, 0, len(p.Palette.Entries))
			for _, e := range p.Palette.Entries {
				eg, _, _ := PPMToScale(e.GainPPM, ScalePermyriad, "floor", "gain")
				out = append(out, map[string]any{"entry_index": e.Index, "entry_id": e.ID, "display_name": e.Name, "group_name": e.Group, "color_hex": hexColor(e.Color), "gain_permyriad": eg})
			}
			return out
		}(),
		"frames": func() []any {
			out := make([]any, 0, len(p.Images))
			for _, im := range p.Images {
				out = append(out, map[string]any{"image_id": im.ID, "width": im.Width, "height": im.Height, "pixels_rgb_base64": encodeFramePixels(im.Pixels), "sha256": im.SHA256})
			}
			return out
		}(),
		"blocks": func() []any {
			out := make([]any, 0, len(p.Blocks))
			for _, b := range p.Blocks {
				out = append(out, map[string]any{
					"block_id": b.ID, "image_id": b.ImageID, "ordinal": b.Index,
					"block_col": b.Col, "block_row": b.Row, "block_width": b.W, "block_height": b.H, "kind": string(b.Kind),
				})
			}
			return out
		}(),
	}
}

func exportV3(p *Project) map[string]any {
	q, _, _ := PPMToScale(p.Params.QuantScalePPM, ScalePermyriad, "floor", "quant")
	g, _, _ := PPMToScale(p.Params.GainPPM, ScalePermyriad, "floor", "gain")
	d, _, _ := PPMToScale(p.Params.GlobalDitherPPM, ScalePermyriad, "floor", "dither")
	familyOrder := []string{}
	familySwatches := map[string][]any{}
	for _, e := range p.Palette.Entries {
		group := e.Group
		if group == "" {
			group = "ungrouped"
		}
		if _, ok := familySwatches[group]; !ok {
			familyOrder = append(familyOrder, group)
		}
		eg, _, _ := PPMToScale(e.GainPPM, ScalePermyriad, "floor", "gain")
		familySwatches[group] = append(familySwatches[group], map[string]any{"index": e.Index, "swatchID": e.ID, "label": e.Name, "hex": hexColor(e.Color), "gainPermyriad": eg})
	}
	families := make([]any, 0, len(familyOrder))
	for _, group := range familyOrder {
		families = append(families, map[string]any{"familyId": group, "familyName": group, "swatches": familySwatches[group]})
	}
	perBlock := []any{}
	for _, b := range p.Blocks {
		if b.HasDitherOverride {
			bd, _, _ := PPMToScale(b.DitherPPM, ScalePermyriad, "floor", "blockDither")
			perBlock = append(perBlock, map[string]any{"blockId": b.ID, "permyriad": bd})
		}
	}
	return map[string]any{
		"schemaVersion":       SchemaV3,
		"projectID":           p.ProjectID,
		"seed":                p.Seed,
		"metric":              string(p.Params.Metric),
		"quantScalePermyriad": q,
		"gainScalePermyriad":  g,
		"grid":                map[string]any{"originX": p.OriginX, "originY": p.OriginY, "tileWidth": p.TileW, "tileHeight": p.TileH, "coordinateExpression": "block_index_v1", "coordDeriveVersion": CoordDeriveVersion},
		"tolerance":           map[string]any{"maxComponentDeviation": p.Tolerance.MaxComponentDeviation, "maxWeightedColorDiff": p.Tolerance.MaxWeightedColorDiff},
		"paletteTree":         map[string]any{"version": p.Palette.Version, "organization": "color_family_tree", "families": families},
		"dither":              map[string]any{"globalPermyriad": d, "perBlock": perBlock},
		"frames": func() []any {
			out := make([]any, 0, len(p.Images))
			for _, im := range p.Images {
				out = append(out, map[string]any{"frameID": im.ID, "width": im.Width, "height": im.Height, "rgbBase64": encodeFramePixels(im.Pixels), "sha256": im.SHA256})
			}
			return out
		}(),
		"cells": func() []any {
			out := make([]any, 0, len(p.Blocks))
			for _, b := range p.Blocks {
				out = append(out, map[string]any{"cellID": b.ID, "frameID": b.ImageID, "ordinal": b.Index, "col": b.Col, "row": b.Row, "width": b.W, "height": b.H, "kind": string(b.Kind)})
			}
			return out
		}(),
	}
}

func SemanticHash(p *Project) string {
	if p == nil {
		return ""
	}
	cp := cloneProject(p)
	cp.Schema = ""
	cp.SourceSchema = ""
	cp.Traces = nil
	// Source/version provenance is intentionally not part of semantic equality.
	cp.Params.ParamVersion = ParamFormatVersion
	cp.Params.AlgoVersion = AlgoVersion
	cp.Params.CoordDeriveVersion = CoordDeriveVersion
	return CanonicalHash(cp)
}
