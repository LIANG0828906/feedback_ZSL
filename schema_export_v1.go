package vpf

import "fmt"

func exportV1(p *Project, report *LossReport, conversions *TraceBook) (map[string]any, error) {
	reportV1Groups(p.Root, "$.layerTree", report)

	layers := make([]any, 0, len(p.Resolved))
	for i, resolved := range p.Resolved {
		layer := resolved.Layer
		path := fmt.Sprintf("$.layers[%d]", i)
		opacityPct := layer.OpacityBP / 100
		if layer.OpacityBP%100 != 0 {
			report.Items = append(report.Items, LossItem{
				Code: CodeLossy, Kind: "opacity_scale", FieldPath: path + ".opacity_pct", SourceID: layer.ID,
				Disposition: "floor_basis_points_to_percent", PrecisionLoss: true,
				Detail: fmt.Sprintf("%d BP is not an exact percent multiple", layer.OpacityBP),
			})
		}
		color, colorPrecisionLoss, err := premultipliedArrayForExport(path+".color_rgba", layer.Color, conversions)
		if err != nil {
			return nil, err
		}
		report.Items = append(report.Items, LossItem{
			Code: CodeLossy, Kind: "alpha_convention", FieldPath: path + ".color_rgba", SourceID: layer.ID,
			Disposition:   "canonical_straight_to_v1_premultiplied_floor",
			PrecisionLoss: colorPrecisionLoss,
		})
		ripples := make([]any, 0, len(layer.Ripples))
		for _, ripple := range layer.Ripples {
			ripples = append(ripples, map[string]any{
				"source_id":   ripple.ID,
				"center_px":   []int{ripple.Center.X, ripple.Center.Y},
				"radius_px":   ripple.Radius,
				"decay_bp":    ripple.DecayBP,
				"strength_bp": ripple.AmplitudeBP,
			})
		}
		layers = append(layers, map[string]any{
			"layer_id":       layer.ID,
			"display_name":   layer.Name,
			"kind":           string(layer.Kind),
			"bounds":         []int{layer.Rect.X, layer.Rect.Y, layer.Rect.W, layer.Rect.H},
			"visible_flag":   resolved.EffectiveVisible,
			"opacity_pct":    opacityPct,
			"blend_code":     exportV1Blend(layer.Blend),
			"summary":        layer.ContentSummary,
			"color_rgba":     color,
			"ripple_sources": ripples,
		})
	}

	frames, commands, err := exportTimeList(p, SchemaV1, conversions)
	if err != nil {
		return nil, err
	}
	return map[string]any{
		"format":         "VPF-FG",
		"version":        SchemaV1,
		"project_id":     p.ProjectID,
		"seed":           p.Seed,
		"frame_width":    p.Width,
		"frame_height":   p.Height,
		"time_unit":      "tick_half_ms",
		"alpha_encoding": "premultiplied_8",
		"layers":         layers,
		"frames":         frames,
		"commands":       commands,
	}, nil
}

func reportV1Groups(node Node, path string, report *LossReport) {
	if node.Kind == NodeGroup && node.ID != "flat_layer_table" && node.ID != "v2_flat_node_table" {
		report.Items = append(report.Items, LossItem{
			Code: CodeLossy, Kind: "group_hierarchy", FieldPath: path, SourceID: node.ID,
			Disposition: "flatten_depth_first_and_bake_ancestor_visibility",
			Detail:      "v1 has only a flat ordered layer table",
		})
	}
	childPath := path + ".children"
	for i, child := range node.Children {
		reportV1Groups(child, fmt.Sprintf("%s[%d]", childPath, i), report)
	}
}

func premultipliedArrayForExport(field string, color RGBA, traces *TraceBook) ([]int, bool, error) {
	precisionLoss := false
	for _, channel := range []uint8{color.R, color.G, color.B} {
		if (int64(channel)*int64(color.A))%ColorMax != 0 {
			precisionLoss = true
		}
	}
	r, err := PremultiplyChannel(field+".r", color.R, color.A, traces)
	if err != nil {
		return nil, false, err
	}
	g, err := PremultiplyChannel(field+".g", color.G, color.A, traces)
	if err != nil {
		return nil, false, err
	}
	b, err := PremultiplyChannel(field+".b", color.B, color.A, traces)
	if err != nil {
		return nil, false, err
	}
	return []int{int(r), int(g), int(b), int(color.A)}, precisionLoss, nil
}

func exportV1Blend(mode BlendMode) string {
	switch mode {
	case BlendAdd:
		return "plus"
	case BlendMultiply:
		return "product"
	default:
		return "alpha"
	}
}
