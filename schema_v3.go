package vpf

import "strings"

func loadV3(root map[string]any) (*Project, error) {
	const version = SchemaV3
	traces := TraceBook{}

	projectID, err := requiredString(root, "projectId", "$", version)
	if err != nil {
		return nil, err
	}
	seed64, err := requiredInt(root, "seed", "$", version)
	if err != nil {
		return nil, err
	}
	canvas, canvasPath, err := requiredObject(root, "canvas", "$", version)
	if err != nil {
		return nil, err
	}
	width64, err := requiredInt(canvas, "width", canvasPath, version)
	if err != nil {
		return nil, err
	}
	height64, err := requiredInt(canvas, "height", canvasPath, version)
	if err != nil {
		return nil, err
	}
	channels64, err := optionalIntDefault(canvas, "channels", canvasPath, version, 4, &traces, "$.channels")
	if err != nil {
		return nil, err
	}
	if channels64 != 4 {
		return nil, schemaError(version, joinField(canvasPath, "channels"), "only RGBA four-channel output is declared")
	}
	tileWidth64, err := optionalIntDefault(canvas, "tileWidth", canvasPath, version, 32, &traces, "$.tile_width")
	if err != nil {
		return nil, err
	}
	tileHeight64, err := optionalIntDefault(canvas, "tileHeight", canvasPath, version, 32, &traces, "$.tile_height")
	if err != nil {
		return nil, err
	}

	alphaConvention, err := requiredString(root, "alphaConvention", "$", version)
	if err != nil {
		return nil, err
	}
	if alphaConvention != "ASSOCIATED" {
		return nil, schemaError(version, "$.alphaConvention", "v3 sample exporter uses ASSOCIATED/premultiplied channels")
	}

	rawTree, _, err := requiredObject(root, "layerTree", "$", version)
	if err != nil {
		return nil, err
	}
	layers := []Layer{}
	tree, err := loadV3Node(rawTree, "$.layerTree", version, &traces, &layers)
	if err != nil {
		return nil, err
	}

	rawFrames, framesPath, err := requiredArray(root, "frameList", "$", version)
	if err != nil {
		return nil, err
	}
	frames, err := loadV3Frames(rawFrames, framesPath, version, &traces)
	if err != nil {
		return nil, err
	}
	rawInstructions, instructionsPath, err := requiredArray(root, "compositeInstructions", "$", version)
	if err != nil {
		return nil, err
	}
	instructions, err := loadV3Instructions(rawInstructions, instructionsPath, version, &traces)
	if err != nil {
		return nil, err
	}

	p := &Project{
		ProjectID:     projectID,
		Seed:          seed64,
		SourceVersion: version,
		Width:         int(width64),
		Height:        int(height64),
		Channels:      int(channels64),
		TileWidth:     int(tileWidth64),
		TileHeight:    int(tileHeight64),
		Frames:        frames,
		Instructions:  instructions,
		Layers:        layers,
		Root:          tree,
		Traces:        traces,
	}
	traceConvert(&p.Traces, version, "$.alphaConvention", "$.color", "associated_to_straight", alphaConvention, AlphaStraight)
	if err := finalizeLoadedProject(p); err != nil {
		return nil, err
	}
	return p, nil
}

func loadV3Node(obj map[string]any, path string, version int, traces *TraceBook, layers *[]Layer) (Node, error) {
	id, err := requiredString(obj, "nodeId", path, version)
	if err != nil {
		return Node{}, err
	}
	typeString, err := requiredString(obj, "nodeType", path, version)
	if err != nil {
		return Node{}, err
	}
	visible, err := requiredBool(obj, "visible", path, version)
	if err != nil {
		return Node{}, err
	}
	label, _ := optionalStringDefault(obj, "label", path, version, id, traces, "$.nodes[].label")

	node := Node{ID: id, Label: label, Visible: visible}
	switch strings.ToUpper(typeString) {
	case "GROUP":
		node.Kind = NodeGroup
		rawChildren, exists := obj["children"]
		if !exists || rawChildren == nil {
			return node, nil
		}
		children, ok := asArray(rawChildren)
		childrenPath := joinField(path, "children")
		if !ok {
			return Node{}, schemaError(version, childrenPath, "children must be an array")
		}
		for i, rawChild := range children {
			childObj, ok := asObject(rawChild)
			childPath := indexField(childrenPath, i)
			if !ok {
				return Node{}, schemaError(version, childPath, "child must be object")
			}
			child, err := loadV3Node(childObj, childPath, version, traces, layers)
			if err != nil {
				return Node{}, err
			}
			node.Children = append(node.Children, child)
		}
	case "LAYER":
		node.Kind = NodeLayer
		rawLayer, layerPath, err := requiredObject(obj, "layer", path, version)
		if err != nil {
			return Node{}, err
		}
		layer, err := loadV3Layer(rawLayer, layerPath, id, visible, version, traces)
		if err != nil {
			return Node{}, err
		}
		*layers = append(*layers, layer)
		node.LayerID = layer.ID
	default:
		return Node{}, schemaError(version, joinField(path, "nodeType"), "unknown v3 node type %q", typeString)
	}
	return node, nil
}

func loadV3Layer(obj map[string]any, path, nodeID string, visible bool, version int, traces *TraceBook) (Layer, error) {
	id, err := requiredString(obj, "layerId", path, version)
	if err != nil {
		return Layer{}, err
	}
	if id != nodeID {
		return Layer{}, schemaError(version, joinField(path, "layerId"), "layerId must match nodeId %q", nodeID)
	}
	name, _ := optionalStringDefault(obj, "displayName", path, version, id, traces, "$.layers[].name")
	typeString, err := requiredString(obj, "layerType", path, version)
	if err != nil {
		return Layer{}, err
	}
	kind := LayerKind(typeString)
	switch kind {
	case KindGround, KindRipple, KindSolid, KindGuide:
	default:
		return Layer{}, schemaError(version, joinField(path, "layerType"), "unknown layer type %q", typeString)
	}
	traceConvert(traces, version, joinField(path, "layerType"), "$.layers[].kind", "camel_enum_direct", typeString, kind)
	boundsObj, boundsPath, err := requiredObject(obj, "bounds", path, version)
	if err != nil {
		return Layer{}, err
	}
	rect, err := parseV3Bounds(boundsObj, boundsPath, version)
	if err != nil {
		return Layer{}, err
	}
	traceConvert(traces, version, boundsPath, "$.layers[].rect", "camel_bounds_to_rect", nil, rect)
	opacityPermille, err := optionalIntDefault(obj, "opacityPermille", path, version, 1000, traces, "$.layers[].opacity_bp")
	if err != nil {
		return Layer{}, err
	}
	if opacityPermille < 0 || opacityPermille > 1000 {
		return Layer{}, schemaError(version, joinField(path, "opacityPermille"), "opacityPermille must be 0..1000")
	}
	opacityBP := int(opacityPermille * 10)
	traceConvert(traces, version, joinField(path, "opacityPermille"), "$.layers[].opacity_bp", "permille_times_10", opacityPermille, opacityBP)

	modeString, err := optionalStringDefault(obj, "blendMode", path, version, "NORMAL", traces, "$.layers[].blend")
	if err != nil {
		return Layer{}, err
	}
	blend, err := mapV3Blend(modeString, joinField(path, "blendMode"), version)
	if err != nil {
		return Layer{}, err
	}
	traceConvert(traces, version, joinField(path, "blendMode"), "$.layers[].blend", "v3_blend_alias", modeString, blend)
	summary, err := requiredString(obj, "contentSummary", path, version)
	if err != nil {
		return Layer{}, err
	}
	traceConvert(traces, version, joinField(path, "contentSummary"), "$.layers[].content_summary", "field_rename", summary, summary)
	colorObj, colorPath, err := requiredObject(obj, "color", path, version)
	if err != nil {
		return Layer{}, err
	}
	color, err := parseV3AssociatedColor(colorObj, colorPath, version, traces)
	if err != nil {
		return Layer{}, err
	}
	ripples, err := parseV3Ripples(obj, path, version)
	if err != nil {
		return Layer{}, err
	}
	return Layer{
		ID: id, Name: name, Kind: kind, Rect: rect, Visible: visible,
		OpacityBP: opacityBP, Blend: blend, Color: color, ContentSummary: summary,
		Ripples: ripples, SourcePath: path, AlphaConvention: AlphaPremultiplied,
	}, nil
}

func parseV3Bounds(obj map[string]any, path string, version int) (Rect, error) {
	x64, err := requiredInt(obj, "x", path, version)
	if err != nil {
		return Rect{}, err
	}
	y64, err := requiredInt(obj, "y", path, version)
	if err != nil {
		return Rect{}, err
	}
	w64, err := requiredInt(obj, "width", path, version)
	if err != nil {
		return Rect{}, err
	}
	h64, err := requiredInt(obj, "height", path, version)
	if err != nil {
		return Rect{}, err
	}
	return Rect{X: int(x64), Y: int(y64), W: int(w64), H: int(h64)}, nil
}

func parseV3AssociatedColor(obj map[string]any, path string, version int, traces *TraceBook) (RGBA, error) {
	r64, err := requiredInt(obj, "r", path, version)
	if err != nil {
		return RGBA{}, err
	}
	g64, err := requiredInt(obj, "g", path, version)
	if err != nil {
		return RGBA{}, err
	}
	b64, err := requiredInt(obj, "b", path, version)
	if err != nil {
		return RGBA{}, err
	}
	a64, err := requiredInt(obj, "alpha", path, version)
	if err != nil {
		return RGBA{}, err
	}
	raw := [4]int64{r64, g64, b64, a64}
	for i, n := range raw {
		if n < 0 || n > 255 {
			return RGBA{}, schemaError(version, indexField(path, i), "channel must be 0..255")
		}
	}
	alpha := uint8(a64)
	out := RGBA{A: alpha}
	values := []*uint8{&out.R, &out.G, &out.B}
	for i := 0; i < 3; i++ {
		field := indexField(path, i)
		straight, err := UnpremultiplyChannel(field, uint8(raw[i]), alpha, traces)
		if err != nil {
			return RGBA{}, err
		}
		*values[i] = straight
		traceConvert(traces, version, field, "$.layers[].color", "associated_unpremultiply_floor", raw[i], straight)
	}
	return out, nil
}

func mapV3Blend(mode, path string, version int) (BlendMode, error) {
	switch strings.ToUpper(mode) {
	case "NORMAL":
		return BlendNormal, nil
	case "ADD":
		return BlendAdd, nil
	case "MULTIPLY":
		return BlendMultiply, nil
	default:
		return "", schemaError(version, path, "unknown v3 blendMode %q", mode)
	}
}

func parseV3Ripples(layerObj map[string]any, layerPath string, version int) ([]RippleSource, error) {
	rawValue, exists := layerObj["rippleSources"]
	if !exists || rawValue == nil {
		return nil, nil
	}
	arr, ok := asArray(rawValue)
	path := joinField(layerPath, "rippleSources")
	if !ok {
		return nil, schemaError(version, path, "rippleSources must be array")
	}
	result := make([]RippleSource, 0, len(arr))
	for i, raw := range arr {
		obj, ok := asObject(raw)
		base := indexField(path, i)
		if !ok {
			return nil, schemaError(version, base, "ripple source must be object")
		}
		id, err := requiredString(obj, "sourceId", base, version)
		if err != nil {
			return nil, err
		}
		x64, err := requiredInt(obj, "centerX", base, version)
		if err != nil {
			return nil, err
		}
		y64, err := requiredInt(obj, "centerY", base, version)
		if err != nil {
			return nil, err
		}
		radius64, err := requiredInt(obj, "radius", base, version)
		if err != nil {
			return nil, err
		}
		decay64, err := requiredInt(obj, "decayBasisPoints", base, version)
		if err != nil {
			return nil, err
		}
		strength64, err := requiredInt(obj, "strengthBasisPoints", base, version)
		if err != nil {
			return nil, err
		}
		result = append(result, RippleSource{
			ID: id, Center: Point{X: int(x64), Y: int(y64)}, Radius: int(radius64),
			DecayBP: int(decay64), AmplitudeBP: int(strength64),
		})
	}
	return result, nil
}

func loadV3Frames(arr []any, path string, version int, traces *TraceBook) ([]Frame, error) {
	frames := make([]Frame, 0, len(arr))
	for i, raw := range arr {
		obj, ok := asObject(raw)
		base := indexField(path, i)
		if !ok {
			return nil, schemaError(version, base, "frame entry must be object")
		}
		index64, err := requiredInt(obj, "frameIndex", base, version)
		if err != nil {
			return nil, err
		}
		us64, err := requiredInt(obj, "timeUs", base, version)
		if err != nil {
			return nil, err
		}
		atNS, err := TimeFromSchemaUnit(joinField(base, "timeUs"), us64, version, traces)
		if err != nil {
			return nil, err
		}
		label, _ := optionalStringDefault(obj, "label", base, version, "", traces, "$.frames[].label")
		frames = append(frames, Frame{Index: int(index64), AtNS: atNS, Label: label})
	}
	return frames, nil
}

func loadV3Instructions(arr []any, path string, version int, traces *TraceBook) ([]Instruction, error) {
	result := make([]Instruction, 0, len(arr))
	for i, raw := range arr {
		obj, ok := asObject(raw)
		base := indexField(path, i)
		if !ok {
			return nil, schemaError(version, base, "instruction must be object")
		}
		order64, err := requiredInt(obj, "order", base, version)
		if err != nil {
			return nil, err
		}
		us64, err := requiredInt(obj, "atUs", base, version)
		if err != nil {
			return nil, err
		}
		atNS, err := TimeFromSchemaUnit(joinField(base, "atUs"), us64, version, traces)
		if err != nil {
			return nil, err
		}
		op, err := requiredString(obj, "operation", base, version)
		if err != nil {
			return nil, err
		}
		layerID, err := requiredString(obj, "layerId", base, version)
		if err != nil {
			return nil, err
		}
		sourceID, _ := optionalStringDefault(obj, "sourceId", base, version, "", traces, "$.instructions[].source_id")
		radius64, _ := optionalIntDefault(obj, "radius", base, version, 0, traces, "$.instructions[].radius")
		strength64, _ := optionalIntDefault(obj, "strengthBasisPoints", base, version, 0, traces, "$.instructions[].amplitude_bp")
		result = append(result, Instruction{
			Order: int(order64), AtNS: atNS, Op: op, LayerID: layerID, SourceID: sourceID,
			Radius: int(radius64), AmplitudeBP: int(strength64),
		})
	}
	return result, nil
}
