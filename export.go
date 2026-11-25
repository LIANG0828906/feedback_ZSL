package vpcompose

import (
	"encoding/json"
	"fmt"
	"strconv"
)

type ExportOptions struct {
	AllowLossy bool
}

type ExportResult struct {
	Data   []byte
	Losses []Loss
	Traces []ConversionTrace
}

func ExportProject(project *Project, target Version) (*ExportResult, error) {
	return ExportProjectWithOptions(project, target, ExportOptions{})
}

func ExportProjectWithOptions(project *Project, target Version, options ExportOptions) (*ExportResult, error) {
	if project == nil {
		return nil, schemaError("project", "nil project")
	}
	if !target.Valid() {
		return nil, schemaError("targetVersion", "unknown schema version")
	}
	clone := project.Clone()
	if err := clone.Normalize(); err != nil {
		return nil, err
	}
	if err := clone.ValidateHierarchy(); err != nil {
		return nil, err
	}
	result := &ExportResult{}
	var (
		data []byte
		err  error
	)
	switch target {
	case Version1:
		data, err = exportV1(clone, result)
	case Version2:
		data, err = exportV2(clone, result)
	case Version3:
		data, err = exportV3(clone, result)
	}
	if err != nil {
		return nil, err
	}
	result.Data = data
	if len(result.Losses) > 0 && !options.AllowLossy {
		ee := &EngineError{
			Code: CodeLossy, Path: "targetVersion",
			Message: fmt.Sprintf("export to %s cannot represent every value", target),
			Losses:  append([]Loss(nil), result.Losses...),
		}
		return result, ee
	}
	return result, nil
}

func exportV1(p *Project, result *ExportResult) ([]byte, error) {
	if hasHierarchy(p) {
		result.Losses = append(result.Losses, Loss{
			Path: "nodes", Kind: "hierarchy", Disposition: "flatten-preorder",
			Detail: "v1 has no group nodes; layers are emitted in existing visual stack order",
		})
	}
	for i, layer := range p.Layers {
		if layer.SourceAlphaMode == AlphaPremultiplied {
			result.Losses = append(result.Losses, Loss{
				Path: fmt.Sprintf("layers[%d].alphaMode", i), Kind: "alpha-convention",
				Disposition: "convert-premultiplied-to-straight",
				Detail:      "v1 declares straight alpha only; the original premultiplied convention is not representable",
			})
			result.Traces = append(result.Traces, ConversionTrace{
				Path:     fmt.Sprintf("layers[%d].alphaMode", i),
				OldValue: string(AlphaPremultiplied), NewValue: string(AlphaStraight),
				Rule: "downgrade-alpha-mode",
			})
		}
	}

	layers := make([]map[string]any, 0, len(p.Layers))
	for i, layer := range p.Layers {
		endMS, err := TicksToMilliseconds(layer.VisibleUntil, p.Timebase.TickRate, p.Timebase.Rounding, fmt.Sprintf("layers[%d].visible_until_ms", i))
		if err != nil {
			return nil, err
		}
		if at, err := MillisecondsToTicks(endMS, p.Timebase.TickRate, p.Timebase.Rounding, fmt.Sprintf("layers[%d].visible_until_ms", i)); err != nil {
			return nil, err
		} else if at != layer.VisibleUntil {
			result.Losses = append(result.Losses, Loss{
				Path: fmt.Sprintf("layers[%d].visibleUntilTick", i), Kind: "timebase",
				Disposition: "round-to-nearest-millisecond",
				Detail:      fmt.Sprintf("%d ticks -> %d ms -> %d ticks", layer.VisibleUntil, endMS, at),
			})
		}
		result.Traces = append(result.Traces, ConversionTrace{
			Path:     fmt.Sprintf("layers[%d].visible_until_ms", i),
			OldValue: strconv.FormatInt(layer.VisibleUntil, 10),
			NewValue: strconv.FormatInt(endMS, 10), Rule: "tick->ms",
		})
		layers = append(layers, map[string]any{
			"layer_id":         layer.ID,
			"name":             layer.Name,
			"content_sha256":   layer.ContentHash,
			"opacity":          int(layer.Opacity),
			"blend_mode":       string(layer.BlendMode),
			"enabled":          layer.Visible,
			"visible_until_ms": int(endMS),
		})
	}
	instructions, err := exportInstructions(p, Version1, result)
	if err != nil {
		return nil, err
	}
	frames, err := exportFrames(p, Version1, result)
	if err != nil {
		return nil, err
	}
	out := map[string]any{
		"format":         "vp-foreground",
		"schema_version": 1,
		"project_id":     p.ID,
		"frame": map[string]any{
			"width": p.Width, "height": p.Height, "channels": p.Channels,
		},
		"timebase": map[string]any{
			"unit": "ms", "tick_rate": p.Timebase.TickRate, "rounding": string(p.Timebase.Rounding),
		},
		"window_size":  p.WindowSize,
		"layers":       layers,
		"instructions": instructions,
		"frames":       frames,
		"truth":        exportTruth(p),
	}
	return json.MarshalIndent(out, "", "  ")
}

func exportV2(p *Project, result *ExportResult) ([]byte, error) {
	nodes := p.Nodes
	if len(nodes) == 0 {
		nodes = flatLayerNodes(p)
	}
	outNodes := make([]map[string]any, 0, len(nodes))
	for i, node := range nodes {
		m := map[string]any{
			"nodeId": node.ID,
			"kind":   string(node.Kind),
			"name":   node.Name,
		}
		if node.ParentID != "" {
			m["parentId"] = node.ParentID
		}
		if node.Kind == NodeLayer {
			layer := p.LayerByID(node.LayerID)
			if layer == nil {
				return nil, schemaError(fmt.Sprintf("nodes[%d].layerId", i), "does not reference a layer")
			}
			m["layerId"] = layer.ID
			m["contentSha256"] = layer.ContentHash
			m["opacity"] = int(layer.Opacity)
			m["blendMode"] = string(layer.BlendMode)
			m["visible"] = layer.Visible
			m["visibleUntilTick"] = layer.VisibleUntil
			m["alphaMode"] = string(AlphaPremultiplied)
		}
		outNodes = append(outNodes, m)
	}
	instructions, err := exportInstructions(p, Version2, result)
	if err != nil {
		return nil, err
	}
	frames, err := exportFrames(p, Version2, result)
	if err != nil {
		return nil, err
	}
	out := map[string]any{
		"format":        "vp-foreground",
		"schemaVersion": 2,
		"projectId":     p.ID,
		"frameSize": map[string]any{
			"width": p.Width, "height": p.Height, "channels": p.Channels,
		},
		"timebase": map[string]any{
			"unit": "tick", "tickRate": p.Timebase.TickRate, "rounding": string(p.Timebase.Rounding),
		},
		"windowSize":   p.WindowSize,
		"nodes":        outNodes,
		"instructions": instructions,
		"frames":       frames,
		"truth":        exportTruth(p),
	}
	return json.MarshalIndent(out, "", "  ")
}

func exportV3(p *Project, result *ExportResult) ([]byte, error) {
	nodeIndex := buildNodeIndex(p)
	children := make(map[string][]string)
	var roots []string
	for _, node := range p.Nodes {
		if node.ParentID == "" {
			roots = append(roots, node.ID)
		} else {
			children[node.ParentID] = append(children[node.ParentID], node.ID)
		}
	}
	if len(roots) == 0 && len(p.Layers) > 0 {
		for _, layer := range p.Layers {
			roots = append(roots, layer.ID)
		}
	}
	var build func(string) (map[string]any, error)
	build = func(id string) (map[string]any, error) {
		node, ok := nodeIndex[id]
		if !ok {
			layer := p.LayerByID(id)
			if layer == nil {
				return nil, schemaError("nodes", "unknown node "+id)
			}
			node = &Node{ID: id, Kind: NodeLayer, LayerID: id, Name: layer.Name}
		}
		switch node.Kind {
		case NodeGroup:
			childMaps := make([]map[string]any, 0, len(children[id]))
			for _, childID := range children[id] {
				child, err := build(childID)
				if err != nil {
					return nil, err
				}
				childMaps = append(childMaps, child)
			}
			return map[string]any{
				"node-id": id, "kind": "group", "name": node.Name,
				"children": childMaps,
			}, nil
		case NodeLayer:
			layer := p.LayerByID(node.LayerID)
			if layer == nil {
				return nil, schemaError("nodes."+id+".layerId", "unknown layer")
			}
			result.Traces = append(result.Traces, ConversionTrace{
				Path:     "layers." + layer.ID + ".alphaMode",
				OldValue: string(layer.SourceAlphaMode), NewValue: string(AlphaPremultiplied),
				Rule: "canonical-alpha-mode",
			})
			return map[string]any{
				"node-id": id, "kind": "layer", "name": node.Name, "layer-id": layer.ID,
				"content":    map[string]any{"sha256": layer.ContentHash},
				"opacity":    int(layer.Opacity),
				"blend-mode": string(layer.BlendMode),
				"visibility": map[string]any{
					"visible": layer.Visible, "until-tick": layer.VisibleUntil,
				},
				"alpha": map[string]any{"mode": string(AlphaPremultiplied)},
			}, nil
		default:
			return nil, schemaError("nodes."+id+".kind", "unsupported node kind")
		}
	}
	tree := make([]map[string]any, 0, len(roots))
	for _, root := range roots {
		node, err := build(root)
		if err != nil {
			return nil, err
		}
		tree = append(tree, node)
	}
	instructions, err := exportInstructions(p, Version3, result)
	if err != nil {
		return nil, err
	}
	frames, err := exportFrames(p, Version3, result)
	if err != nil {
		return nil, err
	}
	out := map[string]any{
		"format":         "vp-foreground",
		"schema-version": 3,
		"project-id":     p.ID,
		"frame-size": map[string]any{
			"width": p.Width, "height": p.Height, "channels": p.Channels,
		},
		"timebase": map[string]any{
			"unit": "tick", "tick-rate": p.Timebase.TickRate, "rounding": string(p.Timebase.Rounding),
		},
		"window-size":  p.WindowSize,
		"tree":         tree,
		"instructions": instructions,
		"frames":       frames,
		"truth":        exportTruth(p),
	}
	return json.MarshalIndent(out, "", "  ")
}

func exportInstructions(p *Project, target Version, result *ExportResult) ([]map[string]any, error) {
	out := make([]map[string]any, 0, len(p.Instructions))
	for i, instruction := range p.Instructions {
		base := fmt.Sprintf("instructions[%d]", i)
		at := instruction.At
		if target == Version1 {
			var err error
			at, err = TicksToMilliseconds(at, p.Timebase.TickRate, p.Timebase.Rounding, base+".at_ms")
			if err != nil {
				return nil, err
			}
			result.Traces = append(result.Traces, ConversionTrace{
				Path: base + ".at_ms", OldValue: strconv.FormatInt(instruction.At, 10),
				NewValue: strconv.FormatInt(at, 10), Rule: "tick->ms",
			})
		}
		m := map[string]any{"sequence": instruction.Sequence}
		if target == Version1 {
			m["at_ms"] = at
			m["action"] = string(instruction.Action)
			m["layer_id"] = instruction.LayerID
		} else if target == Version2 {
			m["seq"] = instruction.Sequence
			m["atTick"] = at
			m["op"] = string(instruction.Action)
			m["layerId"] = instruction.LayerID
		} else {
			m["seq"] = instruction.Sequence
			m["at-tick"] = at
			m["op"] = string(instruction.Action)
			m["layer-id"] = instruction.LayerID
		}
		switch instruction.Action {
		case InstructionSetVisible:
			if target == Version3 {
				m["visible"] = *instruction.Visible
			} else if target == Version2 {
				m["visible"] = *instruction.Visible
			} else {
				m["enabled"] = *instruction.Visible
			}
		case InstructionSetOpacity:
			m["opacity"] = int(*instruction.Opacity)
		case InstructionSetVisibleUntil:
			value := *instruction.VisibleUntil
			if target == Version1 {
				var err error
				rawValue := value
				value, err = TicksToMilliseconds(value, p.Timebase.TickRate, p.Timebase.Rounding, base+".visible_until_ms")
				if err != nil {
					return nil, err
				}
				m["visible_until_ms"] = value
				result.Traces = append(result.Traces, ConversionTrace{
					Path: base + ".visible_until_ms", OldValue: strconv.FormatInt(rawValue, 10),
					NewValue: strconv.FormatInt(value, 10), Rule: "tick->ms",
				})
			} else if target == Version2 {
				m["visibleUntilTick"] = value
			} else {
				m["until-tick"] = value
			}
		case InstructionSetBlendMode:
			m["blend-mode"] = string(instruction.BlendMode)
			if target == Version1 {
				m["blend_mode"] = string(instruction.BlendMode)
				delete(m, "blend-mode")
			} else if target == Version2 {
				m["blendMode"] = string(instruction.BlendMode)
				delete(m, "blend-mode")
			}
		default:
			return nil, schemaError(base+".action", "unsupported action")
		}
		out = append(out, m)
	}
	return out, nil
}

func exportFrames(p *Project, target Version, result *ExportResult) ([]map[string]any, error) {
	out := make([]map[string]any, 0, len(p.Frames))
	for i, frame := range p.Frames {
		base := fmt.Sprintf("frames[%d]", i)
		timeValue := frame.TimeTicks
		timeKey := "timeTick"
		if target == Version1 {
			var err error
			rawTime := frame.TimeTicks
			timeValue, err = TicksToMilliseconds(frame.TimeTicks, p.Timebase.TickRate, p.Timebase.Rounding, base+".time_ms")
			if err != nil {
				return nil, err
			}
			timeKey = "time_ms"
			result.Traces = append(result.Traces, ConversionTrace{
				Path: base + ".time_ms", OldValue: strconv.FormatInt(rawTime, 10),
				NewValue: strconv.FormatInt(timeValue, 10), Rule: "tick->ms",
			})
		} else if target == Version3 {
			timeKey = "time-tick"
		}
		points := make([]map[string]any, 0, len(frame.SamplePoints))
		for _, point := range frame.SamplePoints {
			points = append(points, map[string]any{"x": point.X, "y": point.Y})
		}
		out = append(out, map[string]any{
			"index": frame.Index, timeKey: timeValue, "sample_points": points,
		})
	}
	return out, nil
}

func exportTruth(p *Project) map[string]any {
	if p.Truth == nil {
		return map[string]any{"tolerance": 0, "frames": []any{}}
	}
	frames := make([]map[string]any, 0, len(p.Truth.Frames))
	for _, frame := range p.Truth.Frames {
		pixels := make([]map[string]any, 0, len(frame.SamplePixels))
		for _, pixel := range frame.SamplePixels {
			pixels = append(pixels, map[string]any{
				"x": pixel.X, "y": pixel.Y,
				"rgba": []int{int(pixel.RGBA[0]), int(pixel.RGBA[1]), int(pixel.RGBA[2]), int(pixel.RGBA[3])},
			})
		}
		ends := make(map[string]int64, len(frame.LayerEnds))
		for id, value := range frame.LayerEnds {
			ends[id] = value
		}
		frames = append(frames, map[string]any{
			"index": frame.Index, "timeTick": frame.TimeTicks,
			"layerEnds": ends, "samplePixels": pixels,
		})
	}
	return map[string]any{"tolerance": p.Truth.Tolerance, "frames": frames}
}

func hasHierarchy(p *Project) bool {
	for _, node := range p.Nodes {
		if node.Kind == NodeGroup || node.ParentID != "" {
			return true
		}
	}
	return false
}

func flatLayerNodes(p *Project) []Node {
	nodes := make([]Node, 0, len(p.Layers))
	for _, layer := range p.Layers {
		nodes = append(nodes, Node{ID: layer.ID, Kind: NodeLayer, Name: layer.Name, LayerID: layer.ID})
	}
	return nodes
}

func buildNodeIndex(p *Project) map[string]*Node {
	index := make(map[string]*Node, len(p.Nodes))
	for i := range p.Nodes {
		node := &p.Nodes[i]
		index[node.ID] = node
	}
	return index
}
