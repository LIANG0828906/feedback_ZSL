package vpf

import "fmt"

func isSyntheticRoot(node Node) bool {
	return node.ID == "flat_layer_table" || node.ID == "v2_flat_node_table"
}

func exportV2(p *Project, report *LossReport, conversions *TraceBook) (map[string]any, error) {
	nodes := []any{}
	var walk func(node Node, parentID string) error
	walk = func(node Node, parentID string) error {
		entry := map[string]any{
			"node_id":   node.ID,
			"node_kind": map[NodeKind]string{NodeGroup: "group", NodeLayer: "layer"}[node.Kind],
			"parent_id": parentID,
			"disabled":  !node.Visible,
			"label":     node.Label,
		}
		if node.Kind == NodeLayer {
			layer := lookupLayer(p, node.LayerID)
			entry["layer"] = exportV2Layer(layer)
		}
		nodes = append(nodes, entry)
		for _, child := range node.Children {
			if err := walk(child, node.ID); err != nil {
				return err
			}
		}
		return nil
	}
	if isSyntheticRoot(p.Root) {
		for _, child := range p.Root.Children {
			if err := walk(child, ""); err != nil {
				return nil, err
			}
		}
	} else {
		if err := walk(p.Root, ""); err != nil {
			return nil, err
		}
	}

	frames, directives, err := exportTimeList(p, SchemaV2, conversions)
	if err != nil {
		return nil, err
	}
	return map[string]any{
		"format_version": SchemaV2,
		"meta": map[string]any{
			"project_id": p.ProjectID,
			"seed":       p.Seed,
		},
		"canvas": map[string]any{
			"width":       p.Width,
			"height":      p.Height,
			"channels":    p.Channels,
			"tile_width":  p.TileWidth,
			"tile_height": p.TileHeight,
		},
		"time_base":     "millisecond",
		"alpha_mode":    "straight",
		"nodes":         nodes,
		"frame_records": frames,
		"directives":    directives,
	}, nil
}

func exportV2Layer(layer Layer) map[string]any {
	ripples := make([]any, 0, len(layer.Ripples))
	for _, ripple := range layer.Ripples {
		ripples = append(ripples, map[string]any{
			"id": ripple.ID, "x": ripple.Center.X, "y": ripple.Center.Y,
			"radius": ripple.Radius, "decay_bp": ripple.DecayBP, "strength_bp": ripple.AmplitudeBP,
		})
	}
	return map[string]any{
		"type":       string(layer.Kind),
		"rect":       map[string]any{"x": layer.Rect.X, "y": layer.Rect.Y, "width": layer.Rect.W, "height": layer.Rect.H},
		"opacity_bp": layer.OpacityBP,
		"mode":       exportV2Blend(layer.Blend),
		"content":    layer.ContentSummary,
		"rgba":       map[string]any{"r": layer.Color.R, "g": layer.Color.G, "b": layer.Color.B, "a": layer.Color.A},
		"ripples":    ripples,
	}
}

func exportV2Blend(mode BlendMode) string {
	switch mode {
	case BlendAdd:
		return "add"
	case BlendMultiply:
		return "multiply"
	default:
		return "src-over"
	}
}

func exportV3(p *Project, report *LossReport, conversions *TraceBook) (map[string]any, error) {
	tree, err := exportV3Node(p, p.Root, "$.layerTree", report, conversions)
	if err != nil {
		return nil, err
	}
	frames, instructions, err := exportTimeList(p, SchemaV3, conversions)
	if err != nil {
		return nil, err
	}
	return map[string]any{
		"schema":    "VPF/3.0",
		"version":   SchemaV3,
		"projectId": p.ProjectID,
		"seed":      p.Seed,
		"canvas": map[string]any{
			"width": p.Width, "height": p.Height, "channels": p.Channels,
			"tileWidth": p.TileWidth, "tileHeight": p.TileHeight,
		},
		"timebase":              map[string]any{"unit": "microsecond"},
		"alphaConvention":       "ASSOCIATED",
		"layerTree":             tree,
		"frameList":             frames,
		"compositeInstructions": instructions,
	}, nil
}

func exportV3Node(p *Project, node Node, path string, report *LossReport, conversions *TraceBook) (map[string]any, error) {
	out := map[string]any{
		"nodeId":   node.ID,
		"nodeType": map[NodeKind]string{NodeGroup: "GROUP", NodeLayer: "LAYER"}[node.Kind],
		"visible":  node.Visible,
		"label":    node.Label,
	}
	if node.Kind == NodeLayer {
		layer := lookupLayer(p, node.LayerID)
		layerMap, err := exportV3Layer(path+".layer", layer, report, conversions)
		if err != nil {
			return nil, err
		}
		out["layer"] = layerMap
	}
	if len(node.Children) > 0 {
		children := make([]any, 0, len(node.Children))
		for i, child := range node.Children {
			childMap, err := exportV3Node(p, child, fmt.Sprintf("%s.children[%d]", path, i), report, conversions)
			if err != nil {
				return nil, err
			}
			children = append(children, childMap)
		}
		out["children"] = children
	}
	return out, nil
}

func exportV3Layer(path string, layer Layer, report *LossReport, conversions *TraceBook) (map[string]any, error) {
	opacityPermille := layer.OpacityBP / 10
	if layer.OpacityBP%10 != 0 {
		report.Items = append(report.Items, LossItem{
			Code: CodeLossy, Kind: "opacity_scale", FieldPath: path + ".opacityPermille", SourceID: layer.ID,
			Disposition: "floor_basis_points_to_permille", PrecisionLoss: true,
		})
	}
	premult, precisionLoss, err := premultipliedArrayForExport(path+".color", layer.Color, conversions)
	if err != nil {
		return nil, err
	}
	report.Items = append(report.Items, LossItem{
		Code: CodeLossy, Kind: "alpha_convention", FieldPath: path + ".color", SourceID: layer.ID,
		Disposition:   "canonical_straight_to_v3_associated_floor",
		PrecisionLoss: precisionLoss,
	})
	ripples := make([]any, 0, len(layer.Ripples))
	for _, ripple := range layer.Ripples {
		ripples = append(ripples, map[string]any{
			"sourceId": ripple.ID, "centerX": ripple.Center.X, "centerY": ripple.Center.Y,
			"radius": ripple.Radius, "decayBasisPoints": ripple.DecayBP,
			"strengthBasisPoints": ripple.AmplitudeBP,
		})
	}
	return map[string]any{
		"layerId":         layer.ID,
		"displayName":     layer.Name,
		"layerType":       string(layer.Kind),
		"bounds":          map[string]any{"x": layer.Rect.X, "y": layer.Rect.Y, "width": layer.Rect.W, "height": layer.Rect.H},
		"opacityPermille": opacityPermille,
		"blendMode":       exportV3Blend(layer.Blend),
		"contentSummary":  layer.ContentSummary,
		"color":           map[string]any{"r": premult[0], "g": premult[1], "b": premult[2], "alpha": premult[3]},
		"rippleSources":   ripples,
	}, nil
}

func exportV3Blend(mode BlendMode) string {
	switch mode {
	case BlendAdd:
		return "ADD"
	case BlendMultiply:
		return "MULTIPLY"
	default:
		return "NORMAL"
	}
}
