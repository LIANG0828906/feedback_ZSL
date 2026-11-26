package vpf

func loadV2(root map[string]any) (*Project, error) {
	const version = SchemaV2
	traces := TraceBook{}

	meta, metaPath, err := requiredObject(root, "meta", "$", version)
	if err != nil {
		return nil, err
	}
	projectID, err := requiredString(meta, "project_id", metaPath, version)
	if err != nil {
		return nil, err
	}
	seed, err := requiredInt(meta, "seed", metaPath, version)
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
	tileWidth64, err := optionalIntDefault(canvas, "tile_width", canvasPath, version, 32, &traces, "$.tile_width")
	if err != nil {
		return nil, err
	}
	tileHeight64, err := optionalIntDefault(canvas, "tile_height", canvasPath, version, 32, &traces, "$.tile_height")
	if err != nil {
		return nil, err
	}
	if channels64 != 4 {
		return nil, schemaError(version, joinField(canvasPath, "channels"), "this build declares exactly four RGBA channels")
	}

	alphaMode, err := requiredString(root, "alpha_mode", "$", version)
	if err != nil {
		return nil, err
	}
	if alphaMode != "straight" {
		return nil, schemaError(version, "$.alpha_mode", "v2 loader expects declared straight alpha mode")
	}

	rawNodes, nodesPath, err := requiredArray(root, "nodes", "$", version)
	if err != nil {
		return nil, err
	}
	layers, rootNode, err := loadV2Nodes(rawNodes, nodesPath, version, &traces)
	if err != nil {
		return nil, err
	}

	rawFrames, framesPath, err := requiredArray(root, "frame_records", "$", version)
	if err != nil {
		return nil, err
	}
	frames, err := loadV2Frames(rawFrames, framesPath, version, &traces)
	if err != nil {
		return nil, err
	}

	rawDirectives, directivesPath, err := requiredArray(root, "directives", "$", version)
	if err != nil {
		return nil, err
	}
	instructions, err := loadV2Directives(rawDirectives, directivesPath, version, &traces)
	if err != nil {
		return nil, err
	}

	p := &Project{
		ProjectID:     projectID,
		Seed:          seed,
		SourceVersion: version,
		Width:         int(width64),
		Height:        int(height64),
		Channels:      int(channels64),
		TileWidth:     int(tileWidth64),
		TileHeight:    int(tileHeight64),
		Frames:        frames,
		Instructions:  instructions,
		Layers:        layers,
		Root:          rootNode,
		Traces:        traces,
	}
	traceConvert(&p.Traces, version, "$.alpha_mode", "$.color", "straight_alpha_direct", alphaMode, AlphaStraight)
	if err := finalizeLoadedProject(p); err != nil {
		return nil, err
	}
	return p, nil
}

func loadV2Nodes(rawNodes []any, nodesPath string, version int, traces *TraceBook) ([]Layer, Node, error) {
	layers := []Layer{}
	nodePtrs := map[string]*Node{}
	order := []string{}
	parents := map[string]string{}

	for i, raw := range rawNodes {
		base := indexField(nodesPath, i)
		obj, ok := asObject(raw)
		if !ok {
			return nil, Node{}, schemaError(version, base, "node must be object")
		}
		id, err := requiredString(obj, "node_id", base, version)
		if err != nil {
			return nil, Node{}, err
		}
		kindString, err := requiredString(obj, "node_kind", base, version)
		if err != nil {
			return nil, Node{}, err
		}
		disabled, err := requiredBool(obj, "disabled", base, version)
		if err != nil {
			return nil, Node{}, err
		}
		label, _ := optionalStringDefault(obj, "label", base, version, id, traces, "$.nodes[].label")
		parentID, _ := optionalStringDefault(obj, "parent_id", base, version, "", traces, "$.nodes[].parent_id")
		node := &Node{ID: id, Label: label, Visible: !disabled, ParentHint: parentID}
		parents[id] = parentID
		order = append(order, id)

		switch kindString {
		case "group":
			node.Kind = NodeGroup
		case "layer":
			node.Kind = NodeLayer
			rawLayer, layerPath, lerr := requiredObject(obj, "layer", base, version)
			if lerr != nil {
				return nil, Node{}, lerr
			}
			layer, lerr := loadV2LayerProps(rawLayer, layerPath, id, !disabled, version, traces)
			if lerr != nil {
				return nil, Node{}, lerr
			}
			layers = append(layers, layer)
			node.LayerID = id
		default:
			return nil, Node{}, schemaError(version, joinField(base, "node_kind"), "unknown node kind %q", kindString)
		}
		nodePtrs[id] = node
	}

	root := Node{ID: "v2_flat_node_table", Kind: NodeGroup, Visible: true, Label: "v2 parent-reference table"}
	for _, id := range order {
		node := nodePtrs[id]
		parentID := parents[id]
		if parentID == "" {
			root.Children = append(root.Children, *node)
			continue
		}
		parent, ok := nodePtrs[parentID]
		if !ok {
			return nil, Node{}, schemaError(version, joinField(indexField(nodesPath, indexOfID(order, id)), "parent_id"), "parent node %q does not exist", parentID)
		}
		parent.Children = append(parent.Children, *node)
	}

	// Copy assembled pointers back into root. The first-level values were
	// appended before children were assembled, so rebuild recursively.
	var build func(id string) Node
	build = func(id string) Node {
		src := nodePtrs[id]
		out := *src
		out.Children = nil
		for childID, parentID := range parents {
			if parentID == id {
				// Child order must follow source order, not map iteration.
				_ = childID
			}
		}
		for _, childID := range childrenInOrder(id, order, parents) {
			out.Children = append(out.Children, build(childID))
		}
		return out
	}
	root.Children = nil
	for _, id := range order {
		if parents[id] == "" {
			root.Children = append(root.Children, build(id))
		}
	}
	return layers, root, nil
}

func indexOfID(ids []string, target string) int {
	for i, id := range ids {
		if id == target {
			return i
		}
	}
	return -1
}

func childrenInOrder(parentID string, order []string, parents map[string]string) []string {
	var children []string
	for _, id := range order {
		if parents[id] == parentID {
			children = append(children, id)
		}
	}
	return children
}

func loadV2LayerProps(obj map[string]any, base, id string, visible bool, version int, traces *TraceBook) (Layer, error) {
	typeString, err := requiredString(obj, "type", base, version)
	if err != nil {
		return Layer{}, err
	}
	kind := LayerKind(typeString)
	switch kind {
	case KindGround, KindRipple, KindSolid, KindGuide:
	default:
		return Layer{}, schemaError(version, joinField(base, "type"), "unknown layer type %q", typeString)
	}
	traceConvert(traces, version, joinField(base, "type"), "$.layers[].kind", "snake_enum_direct", typeString, kind)
	rectObj, rectPath, err := requiredObject(obj, "rect", base, version)
	if err != nil {
		return Layer{}, err
	}
	rect, err := parseV2Rect(rectObj, rectPath, version)
	if err != nil {
		return Layer{}, err
	}
	traceConvert(traces, version, rectPath, "$.layers[].rect", "rect_object_direct", nil, rect)
	opacity64, err := optionalIntDefault(obj, "opacity_bp", base, version, OpacityScale, traces, "$.nodes[].opacity_bp")
	if err != nil {
		return Layer{}, err
	}
	if opacity64 < 0 || opacity64 > OpacityScale {
		return Layer{}, schemaError(version, joinField(base, "opacity_bp"), "opacity_bp must be 0..10000")
	}
	traceConvert(traces, version, joinField(base, "opacity_bp"), "$.layers[].opacity_bp", "basis_points_direct", opacity64, opacity64)
	mode, err := optionalStringDefault(obj, "mode", base, version, "src-over", traces, "$.nodes[].blend")
	if err != nil {
		return Layer{}, err
	}
	blend, err := mapV2Blend(mode, joinField(base, "mode"), version)
	if err != nil {
		return Layer{}, err
	}
	traceConvert(traces, version, joinField(base, "mode"), "$.layers[].blend", "v2_blend_alias", mode, blend)
	content, err := requiredString(obj, "content", base, version)
	if err != nil {
		return Layer{}, err
	}
	traceConvert(traces, version, joinField(base, "content"), "$.layers[].content_summary", "field_rename", content, content)
	colorObj, colorPath, err := requiredObject(obj, "rgba", base, version)
	if err != nil {
		return Layer{}, err
	}
	color, err := parseStraightRGBAObject(colorObj, colorPath, version)
	if err != nil {
		return Layer{}, err
	}
	traceConvert(traces, version, colorPath, "$.layers[].color", "straight_rgba_direct", nil, color)

	var ripples []RippleSource
	if raw, exists := obj["ripples"]; exists && raw != nil {
		arr, ok := asArray(raw)
		path := joinField(base, "ripples")
		if !ok {
			return Layer{}, schemaError(version, path, "expected array")
		}
		ripples, err = parseV2Ripples(arr, path, version)
		if err != nil {
			return Layer{}, err
		}
	}

	return Layer{
		ID: id, Name: id, Kind: kind, Rect: rect, Visible: visible,
		OpacityBP: int(opacity64), Blend: blend, Color: color,
		ContentSummary: content, Ripples: ripples, SourcePath: base,
		AlphaConvention: AlphaStraight,
	}, nil
}

func parseV2Rect(obj map[string]any, path string, version int) (Rect, error) {
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
	if w64 <= 0 || h64 <= 0 {
		return Rect{}, schemaError(version, path, "rect width/height must be positive")
	}
	return Rect{X: int(x64), Y: int(y64), W: int(w64), H: int(h64)}, nil
}

func parseStraightRGBAObject(obj map[string]any, path string, version int) (RGBA, error) {
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
	a64, err := optionalIntDefault(obj, "a", path, version, 255, nil, "$.color.a")
	if err != nil {
		return RGBA{}, err
	}
	for key, n := range map[string]int64{"r": r64, "g": g64, "b": b64, "a": a64} {
		if n < 0 || n > 255 {
			return RGBA{}, schemaError(version, joinField(path, key), "channel must be 0..255")
		}
	}
	return RGBA{R: uint8(r64), G: uint8(g64), B: uint8(b64), A: uint8(a64)}, nil
}

func mapV2Blend(mode, path string, version int) (BlendMode, error) {
	switch mode {
	case "src-over":
		return BlendNormal, nil
	case "add":
		return BlendAdd, nil
	case "multiply":
		return BlendMultiply, nil
	default:
		return "", schemaError(version, path, "unknown v2 mode %q", mode)
	}
}

func parseV2Ripples(arr []any, path string, version int) ([]RippleSource, error) {
	result := make([]RippleSource, 0, len(arr))
	for i, raw := range arr {
		obj, ok := asObject(raw)
		base := indexField(path, i)
		if !ok {
			return nil, schemaError(version, base, "ripple must be object")
		}
		id, err := requiredString(obj, "id", base, version)
		if err != nil {
			return nil, err
		}
		x64, err := requiredInt(obj, "x", base, version)
		if err != nil {
			return nil, err
		}
		y64, err := requiredInt(obj, "y", base, version)
		if err != nil {
			return nil, err
		}
		radius64, err := requiredInt(obj, "radius", base, version)
		if err != nil {
			return nil, err
		}
		decay64, err := requiredInt(obj, "decay_bp", base, version)
		if err != nil {
			return nil, err
		}
		strength64, err := requiredInt(obj, "strength_bp", base, version)
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

func loadV2Frames(arr []any, path string, version int, traces *TraceBook) ([]Frame, error) {
	frames := make([]Frame, 0, len(arr))
	for i, raw := range arr {
		obj, ok := asObject(raw)
		base := indexField(path, i)
		if !ok {
			return nil, schemaError(version, base, "frame record must be object")
		}
		index64, err := requiredInt(obj, "frame_index", base, version)
		if err != nil {
			return nil, err
		}
		ms64, err := requiredInt(obj, "time_ms", base, version)
		if err != nil {
			return nil, err
		}
		atNS, err := TimeFromSchemaUnit(joinField(base, "time_ms"), ms64, version, traces)
		if err != nil {
			return nil, err
		}
		marker, _ := optionalStringDefault(obj, "marker", base, version, "", traces, "$.frames[].label")
		frames = append(frames, Frame{Index: int(index64), AtNS: atNS, Label: marker})
	}
	return frames, nil
}

func loadV2Directives(arr []any, path string, version int, traces *TraceBook) ([]Instruction, error) {
	result := make([]Instruction, 0, len(arr))
	for i, raw := range arr {
		obj, ok := asObject(raw)
		base := indexField(path, i)
		if !ok {
			return nil, schemaError(version, base, "directive must be object")
		}
		order64, err := requiredInt(obj, "order", base, version)
		if err != nil {
			return nil, err
		}
		ms64, err := requiredInt(obj, "at_ms", base, version)
		if err != nil {
			return nil, err
		}
		atNS, err := TimeFromSchemaUnit(joinField(base, "at_ms"), ms64, version, traces)
		if err != nil {
			return nil, err
		}
		op, err := requiredString(obj, "verb", base, version)
		if err != nil {
			return nil, err
		}
		target, err := requiredString(obj, "target_layer", base, version)
		if err != nil {
			return nil, err
		}
		sourceID, _ := optionalStringDefault(obj, "source", base, version, "", traces, "$.instructions[].source_id")
		radius64, _ := optionalIntDefault(obj, "radius", base, version, 0, traces, "$.instructions[].radius")
		strength64, _ := optionalIntDefault(obj, "strength_bp", base, version, 0, traces, "$.instructions[].amplitude_bp")
		result = append(result, Instruction{
			Order: int(order64), AtNS: atNS, Op: op, LayerID: target, SourceID: sourceID,
			Radius: int(radius64), AmplitudeBP: int(strength64),
		})
	}
	return result, nil
}
