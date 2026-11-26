package vpf

import "fmt"

func loadV1(root map[string]any) (*Project, error) {
	const version = SchemaV1
	traces := TraceBook{}

	projectID, err := requiredString(root, "project_id", "$", version)
	if err != nil {
		return nil, err
	}
	seed, err := requiredInt(root, "seed", "$", version)
	if err != nil {
		return nil, err
	}
	width64, err := requiredInt(root, "frame_width", "$", version)
	if err != nil {
		return nil, err
	}
	height64, err := requiredInt(root, "frame_height", "$", version)
	if err != nil {
		return nil, err
	}
	tileWidth64, err := optionalIntDefault(root, "tile_width", "$", version, 32, &traces, "$.tile_width")
	if err != nil {
		return nil, err
	}
	tileHeight64, err := optionalIntDefault(root, "tile_height", "$", version, 32, &traces, "$.tile_height")
	if err != nil {
		return nil, err
	}

	rawLayers, layersPath, err := requiredArray(root, "layers", "$", version)
	if err != nil {
		return nil, err
	}
	layers := make([]Layer, 0, len(rawLayers))
	layerNodes := make([]Node, 0, len(rawLayers))
	for i, raw := range rawLayers {
		obj, ok := asObject(raw)
		base := indexField(layersPath, i)
		if !ok {
			return nil, schemaError(version, base, "layer must be an object")
		}
		layer, node, err := loadV1Layer(obj, base, version, &traces)
		if err != nil {
			return nil, err
		}
		layers = append(layers, layer)
		layerNodes = append(layerNodes, node)
	}

	rawFrames, framesPath, err := requiredArray(root, "frames", "$", version)
	if err != nil {
		return nil, err
	}
	frames, err := loadV1Frames(rawFrames, framesPath, version, &traces)
	if err != nil {
		return nil, err
	}

	rawCommands, commandsPath, err := requiredArray(root, "commands", "$", version)
	if err != nil {
		return nil, err
	}
	instructions, err := loadV1Commands(rawCommands, commandsPath, version, &traces)
	if err != nil {
		return nil, err
	}

	p := &Project{
		ProjectID:     projectID,
		Seed:          seed,
		SourceVersion: version,
		Width:         int(width64),
		Height:        int(height64),
		Channels:      4,
		TileWidth:     int(tileWidth64),
		TileHeight:    int(tileHeight64),
		Frames:        frames,
		Instructions:  instructions,
		Layers:        layers,
		Root: Node{
			ID:       "flat_layer_table",
			Kind:     NodeGroup,
			Label:    "v1 flat layer table",
			Visible:  true,
			Children: layerNodes,
		},
		Traces: traces,
	}
	traceConvert(&p.Traces, version, "$.frame_width", "$.width", "int_direct", width64, p.Width)
	traceConvert(&p.Traces, version, "$.frame_height", "$.height", "int_direct", height64, p.Height)
	if err := finalizeLoadedProject(p); err != nil {
		return nil, err
	}
	return p, nil
}

func loadV1Layer(obj map[string]any, base string, version int, traces *TraceBook) (Layer, Node, error) {
	id, err := requiredString(obj, "layer_id", base, version)
	if err != nil {
		return Layer{}, Node{}, err
	}
	name, err := optionalStringDefault(obj, "display_name", base, version, id, traces, "$.layers[].name")
	if err != nil {
		return Layer{}, Node{}, err
	}
	kindString, err := requiredString(obj, "kind", base, version)
	if err != nil {
		return Layer{}, Node{}, err
	}
	kind := LayerKind(kindString)
	switch kind {
	case KindGround, KindRipple, KindSolid, KindGuide:
	default:
		return Layer{}, Node{}, schemaError(version, joinField(base, "kind"), "unknown layer kind %q", kindString)
	}

	rect, err := parseV1Bounds(obj, base, version)
	if err != nil {
		return Layer{}, Node{}, err
	}
	visible, err := requiredBool(obj, "visible_flag", base, version)
	if err != nil {
		return Layer{}, Node{}, err
	}
	opacityPct, err := optionalIntDefault(obj, "opacity_pct", base, version, 100, traces, "$.layers[].opacity_bp")
	if err != nil {
		return Layer{}, Node{}, err
	}
	if opacityPct < 0 || opacityPct > 100 {
		return Layer{}, Node{}, schemaError(version, joinField(base, "opacity_pct"), "opacity percent must be 0..100")
	}
	opacityBP := int(opacityPct * 100)
	traceConvert(traces, version, joinField(base, "opacity_pct"), "$.layers[].opacity_bp", "percent_times_100", opacityPct, opacityBP)

	blendCode, err := optionalStringDefault(obj, "blend_code", base, version, "alpha", traces, "$.layers[].blend")
	if err != nil {
		return Layer{}, Node{}, err
	}
	blend, err := mapV1Blend(blendCode, joinField(base, "blend_code"), version)
	if err != nil {
		return Layer{}, Node{}, err
	}
	summary, err := requiredString(obj, "summary", base, version)
	if err != nil {
		return Layer{}, Node{}, err
	}
	color, err := parseV1PremultipliedColor(obj, base, version, traces)
	if err != nil {
		return Layer{}, Node{}, err
	}

	var ripples []RippleSource
	if raw, exists := obj["ripple_sources"]; exists && raw != nil {
		arr, ok := asArray(raw)
		path := joinField(base, "ripple_sources")
		if !ok {
			return Layer{}, Node{}, schemaError(version, path, "expected array")
		}
		ripples, err = parseV1Ripples(arr, path, version, traces)
		if err != nil {
			return Layer{}, Node{}, err
		}
	}

	layer := Layer{
		ID:              id,
		Name:            name,
		Kind:            kind,
		Rect:            rect,
		Visible:         visible,
		OpacityBP:       opacityBP,
		Blend:           blend,
		Color:           color,
		ContentSummary:  summary,
		Ripples:         ripples,
		SourcePath:      base,
		AlphaConvention: AlphaPremultiplied,
	}
	node := Node{ID: id, Kind: NodeLayer, Label: name, Visible: visible, LayerID: id}
	return layer, node, nil
}

func parseV1Bounds(obj map[string]any, base string, version int) (Rect, error) {
	path := joinField(base, "bounds")
	raw, exists := obj["bounds"]
	if !exists || raw == nil {
		return Rect{}, schemaError(version, path, "required field is missing")
	}
	arr, ok := asArray(raw)
	if !ok || len(arr) != 4 {
		return Rect{}, schemaError(version, path, "bounds must be [x,y,w,h]")
	}
	values := make([]int, 4)
	for i, item := range arr {
		n, ok := integerValue(item)
		if !ok {
			return Rect{}, schemaError(version, indexField(path, i), "expected integer")
		}
		values[i] = int(n)
	}
	if values[2] <= 0 || values[3] <= 0 {
		return Rect{}, schemaError(version, path, "bounds width and height must be positive")
	}
	return Rect{X: values[0], Y: values[1], W: values[2], H: values[3]}, nil
}

func parseV1PremultipliedColor(obj map[string]any, base string, version int, traces *TraceBook) (RGBA, error) {
	path := joinField(base, "color_rgba")
	raw, exists := obj["color_rgba"]
	if !exists || raw == nil {
		return RGBA{}, schemaError(version, path, "required color field is missing")
	}
	arr, ok := asArray(raw)
	if !ok || len(arr) != 4 {
		return RGBA{}, schemaError(version, path, "color_rgba must contain four integers")
	}
	values := [4]uint8{}
	for i, item := range arr {
		n, ok := integerValue(item)
		if !ok || n < 0 || n > 255 {
			return RGBA{}, schemaError(version, indexField(path, i), "color channel must be integer 0..255")
		}
		values[i] = uint8(n)
	}
	alpha := values[3]
	channels := []uint8{0, 0, 0}
	for i := 0; i < 3; i++ {
		field := indexField(path, i)
		straight, err := UnpremultiplyChannel(field, values[i], alpha, traces)
		if err != nil {
			return RGBA{}, err
		}
		channels[i] = straight
		traceConvert(traces, version, field, joinField(base, "color"), "unpremultiply_floor", values[i], straight)
	}
	return RGBA{R: channels[0], G: channels[1], B: channels[2], A: alpha}, nil
}

func mapV1Blend(code, path string, version int) (BlendMode, error) {
	switch code {
	case "alpha":
		return BlendNormal, nil
	case "plus":
		return BlendAdd, nil
	case "product":
		return BlendMultiply, nil
	default:
		return "", schemaError(version, path, "unknown v1 blend_code %q", code)
	}
}

func parseV1Ripples(arr []any, path string, version int, traces *TraceBook) ([]RippleSource, error) {
	result := make([]RippleSource, 0, len(arr))
	for i, raw := range arr {
		obj, ok := asObject(raw)
		base := indexField(path, i)
		if !ok {
			return nil, schemaError(version, base, "ripple source must be object")
		}
		id, err := requiredString(obj, "source_id", base, version)
		if err != nil {
			return nil, err
		}
		centerRaw, centerPath, err := requiredArray(obj, "center_px", base, version)
		if err != nil {
			return nil, err
		}
		if len(centerRaw) != 2 {
			return nil, schemaError(version, centerPath, "center_px must be [x,y]")
		}
		x64, ok := integerValue(centerRaw[0])
		if !ok {
			return nil, schemaError(version, indexField(centerPath, 0), "center x must be integer")
		}
		y64, ok := integerValue(centerRaw[1])
		if !ok {
			return nil, schemaError(version, indexField(centerPath, 1), "center y must be integer")
		}
		radius64, err := requiredInt(obj, "radius_px", base, version)
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
		traceConvert(traces, version, base, "$.layers[].ripples[]", "snake_case_direct", nil, id)
	}
	return result, nil
}

func loadV1Frames(arr []any, path string, version int, traces *TraceBook) ([]Frame, error) {
	frames := make([]Frame, 0, len(arr))
	for i, raw := range arr {
		obj, ok := asObject(raw)
		base := indexField(path, i)
		if !ok {
			return nil, schemaError(version, base, "frame must be object")
		}
		index64, err := requiredInt(obj, "frame_no", base, version)
		if err != nil {
			return nil, err
		}
		tick64, err := requiredInt(obj, "at_tick", base, version)
		if err != nil {
			return nil, err
		}
		atNS, err := TimeFromSchemaUnit(joinField(base, "at_tick"), tick64, version, traces)
		if err != nil {
			return nil, err
		}
		label, _ := optionalStringDefault(obj, "cue", base, version, fmt.Sprintf("frame-%d", index64), traces, "$.frames[].label")
		frames = append(frames, Frame{Index: int(index64), AtNS: atNS, Label: label})
	}
	return frames, nil
}

func loadV1Commands(arr []any, path string, version int, traces *TraceBook) ([]Instruction, error) {
	commands := make([]Instruction, 0, len(arr))
	for i, raw := range arr {
		obj, ok := asObject(raw)
		base := indexField(path, i)
		if !ok {
			return nil, schemaError(version, base, "command must be object")
		}
		seq64, err := requiredInt(obj, "seq", base, version)
		if err != nil {
			return nil, err
		}
		tick64, err := requiredInt(obj, "at_tick", base, version)
		if err != nil {
			return nil, err
		}
		atNS, err := TimeFromSchemaUnit(joinField(base, "at_tick"), tick64, version, traces)
		if err != nil {
			return nil, err
		}
		op, err := requiredString(obj, "op", base, version)
		if err != nil {
			return nil, err
		}
		layerID, err := requiredString(obj, "layer", base, version)
		if err != nil {
			return nil, err
		}
		sourceID, _ := optionalStringDefault(obj, "source", base, version, "", traces, "$.instructions[].source_id")
		radius64, _ := optionalIntDefault(obj, "radius_px", base, version, 0, traces, "$.instructions[].radius")
		strength64, _ := optionalIntDefault(obj, "strength_bp", base, version, 0, traces, "$.instructions[].amplitude_bp")
		commands = append(commands, Instruction{
			Order: int(seq64), AtNS: atNS, Op: op, LayerID: layerID, SourceID: sourceID,
			Radius: int(radius64), AmplitudeBP: int(strength64),
		})
	}
	return commands, nil
}
