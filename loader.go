package vpcompose

import (
	"encoding/json"
	"fmt"
	"io"
	"os"
	"sort"
	"strconv"
	"strings"
)

type Loader struct{}

func LoadProject(data []byte) (*Project, error) {
	return (&Loader{}).Load(data)
}

func LoadProjectFile(path string) (*Project, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, &EngineError{Code: CodeDecode, Path: path, Message: "cannot read project", Cause: err}
	}
	return LoadProject(data)
}

func (l *Loader) Load(data []byte) (*Project, error) {
	decoder := json.NewDecoder(strings.NewReader(string(data)))
	decoder.UseNumber()
	var root any
	if err := decoder.Decode(&root); err != nil {
		return nil, decodeError("$", err)
	}
	var trailing any
	if err := decoder.Decode(&trailing); err != io.EOF {
		if err == nil {
			err = fmt.Errorf("trailing JSON value")
		}
		return nil, decodeError("$", err)
	}
	obj, err := asObject(root, "$")
	if err != nil {
		return nil, err
	}
	if format, ok, err := optionalString(obj, "format", "$"); err != nil {
		return nil, err
	} else if ok && format != "vp-foreground" {
		return nil, schemaError("$.format", "unsupported format "+format)
	}

	version, versionPath, err := detectVersion(obj)
	if err != nil {
		return nil, err
	}
	var project *Project
	switch version {
	case Version1:
		project, err = loadV1(obj)
	case Version2:
		project, err = loadV2(obj)
	case Version3:
		project, err = loadV3(obj)
	default:
		return nil, schemaError(versionPath, "unknown schema version")
	}
	if err != nil {
		return nil, err
	}
	if err := project.Normalize(); err != nil {
		return nil, err
	}
	if err := project.ValidateHierarchy(); err != nil {
		return nil, err
	}
	return project, nil
}

func detectVersion(obj map[string]any) (Version, string, error) {
	candidates := []struct {
		key string
		ver Version
	}{
		{"schema_version", Version1},
		{"schemaVersion", Version2},
		{"schema-version", Version3},
	}
	found := false
	for _, candidate := range candidates {
		raw, ok := obj[candidate.key]
		if !ok {
			continue
		}
		found = true
		value, err := asInt(raw, candidate.key)
		if err != nil {
			return 0, candidate.key, err
		}
		if Version(value) != candidate.ver {
			return 0, candidate.key, schemaError(candidate.key, fmt.Sprintf("unknown schema version %d", value))
		}
		return candidate.ver, candidate.key, nil
	}
	if !found {
		return 0, "schema_version", schemaError("schema_version", "required version field is missing")
	}
	return 0, "", schemaError("schema_version", "unknown schema version")
}

func loadV1(obj map[string]any) (*Project, error) {
	p := &Project{SourceVersion: Version1, SourceFormat: "legacy-flat-straight-ms", ParamVersion: 1}
	if err := parseCommonProjectFields(obj, p, "project_id", "frame", "window_size"); err != nil {
		return nil, err
	}
	tb, err := requiredObject(obj, "timebase", "timebase")
	if err != nil {
		return nil, err
	}
	unit, err := requiredString(tb, "unit", "timebase.unit")
	if err != nil {
		return nil, err
	}
	if unit != "ms" {
		return nil, schemaError("timebase.unit", "v1 requires ms")
	}
	rate, err := requiredInt(tb, "tick_rate", "timebase.tick_rate")
	if err != nil {
		return nil, err
	}
	rounding, err := requiredRounding(tb, "rounding", "timebase.rounding")
	if err != nil {
		return nil, err
	}
	p.Timebase = InternalTimebase(rate)
	p.Timebase.Rounding = rounding

	rawLayers, err := requiredArray(obj, "layers", "layers")
	if err != nil {
		return nil, err
	}
	for i, raw := range rawLayers {
		base := fmt.Sprintf("layers[%d]", i)
		m, err := asObject(raw, base)
		if err != nil {
			return nil, err
		}
		id, err := requiredString(m, "layer_id", base+".layer_id")
		if err != nil {
			return nil, err
		}
		name, _, err := optionalString(m, "name", base+".name")
		if err != nil {
			return nil, err
		}
		content, err := requiredString(m, "content_sha256", base+".content_sha256")
		if err != nil {
			return nil, err
		}
		opacity, err := requiredUint16(m, "opacity", base+".opacity")
		if err != nil {
			return nil, err
		}
		blend, err := requiredString(m, "blend_mode", base+".blend_mode")
		if err != nil {
			return nil, err
		}
		visible, err := requiredBool(m, "enabled", base+".enabled")
		if err != nil {
			return nil, err
		}
		endMS, err := requiredInt(m, "visible_until_ms", base+".visible_until_ms")
		if err != nil {
			return nil, err
		}
		endTicks, err := MillisecondsToTicks(endMS, rate, rounding, base+".visible_until_ms")
		if err != nil {
			return nil, err
		}
		p.ConversionTraces = append(p.ConversionTraces, ConversionTrace{
			Path: base + ".visible_until_ms", OldValue: strconv.FormatInt(endMS, 10),
			NewValue: strconv.FormatInt(endTicks, 10), Rule: "ms->tick",
			Detail: fmt.Sprintf("tick_rate=%d rounding=%s", rate, rounding),
		})
		alpha := AlphaStraight
		if rawAlpha, ok, err := optionalString(m, "alpha_mode", base+".alpha_mode"); err != nil {
			return nil, err
		} else if ok {
			alpha = AlphaMode(rawAlpha)
			if alpha != AlphaStraight {
				return nil, schemaError(base+".alpha_mode", "v1 only supports straight alpha")
			}
		} else {
			p.ConversionTraces = append(p.ConversionTraces, ConversionTrace{
				Path: base + ".alpha_mode", OldValue: "<missing>", NewValue: string(AlphaStraight),
				Rule: "schema-default", Detail: "v1 default alpha convention",
			})
		}
		parent := ""
		if rawParent, ok, err := optionalString(m, "parent_id", base+".parent_id"); err != nil {
			return nil, err
		} else if ok {
			parent = rawParent
		} else {
			p.ConversionTraces = append(p.ConversionTraces, ConversionTrace{
				Path: base + ".parent_id", OldValue: "<missing>", NewValue: "",
				Rule: "schema-default", Detail: "v1 flat table has no hierarchy",
			})
		}
		layer := &Layer{
			ID: id, Name: name, ParentID: parent, ContentHash: content,
			Opacity: opacity, BlendMode: BlendMode(blend), Visible: visible,
			VisibleUntil: endTicks, AlphaMode: AlphaPremultiplied, SourceAlphaMode: alpha,
			ParamVersion: 1,
		}
		if !layer.BlendMode.Valid() {
			return nil, schemaError(base+".blend_mode", "unsupported blend mode "+blend)
		}
		p.Layers = append(p.Layers, layer)
		p.Nodes = append(p.Nodes, Node{ID: id, Kind: NodeLayer, Name: name, LayerID: id})
		p.ConversionTraces = append(p.ConversionTraces, ConversionTrace{
			Path: base + ".alpha_mode", OldValue: string(AlphaStraight),
			NewValue: string(AlphaPremultiplied), Rule: "canonical-alpha-mode",
			Detail: "RGB channels are premultiplied into the internal 16-bit model",
		})
	}

	if err := parseInstructions(obj, p, "instructions", "at_ms", "action", "layer_id", v1InstructionFields); err != nil {
		return nil, err
	}
	if err := parseFrames(obj, p, "frames", "time_ms"); err != nil {
		return nil, err
	}
	if err := parseTruth(obj, p); err != nil {
		return nil, err
	}
	return p, nil
}

func loadV2(obj map[string]any) (*Project, error) {
	p := &Project{SourceVersion: Version2, SourceFormat: "broadcast-flat-groups-premul-tick", ParamVersion: 1}
	if err := parseCommonProjectFields(obj, p, "projectId", "frameSize", "windowSize"); err != nil {
		return nil, err
	}
	tb, err := requiredObject(obj, "timebase", "timebase")
	if err != nil {
		return nil, err
	}
	unit, err := requiredString(tb, "unit", "timebase.unit")
	if err != nil {
		return nil, err
	}
	if unit != "tick" {
		return nil, schemaError("timebase.unit", "v2 requires tick")
	}
	rate, err := requiredInt(tb, "tickRate", "timebase.tickRate")
	if err != nil {
		return nil, err
	}
	rounding, err := requiredRounding(tb, "rounding", "timebase.rounding")
	if err != nil {
		return nil, err
	}
	p.Timebase = InternalTimebase(rate)
	p.Timebase.Rounding = rounding

	rawNodes, err := requiredArray(obj, "nodes", "nodes")
	if err != nil {
		return nil, err
	}
	children := make(map[string][]string)
	for i, raw := range rawNodes {
		base := fmt.Sprintf("nodes[%d]", i)
		m, err := asObject(raw, base)
		if err != nil {
			return nil, err
		}
		nodeID, err := requiredString(m, "nodeId", base+".nodeId")
		if err != nil {
			return nil, err
		}
		kind, err := requiredString(m, "kind", base+".kind")
		if err != nil {
			return nil, err
		}
		name, _, err := optionalString(m, "name", base+".name")
		if err != nil {
			return nil, err
		}
		parent, hasParent, err := optionalString(m, "parentId", base+".parentId")
		if err != nil {
			return nil, err
		}
		if !hasParent {
			p.ConversionTraces = append(p.ConversionTraces, ConversionTrace{
				Path: base + ".parentId", OldValue: "<missing>", NewValue: "",
				Rule: "schema-default", Detail: "v2 default places node at the root",
			})
		}
		node := Node{ID: nodeID, Kind: NodeKind(kind), Name: name, ParentID: parent}
		switch node.Kind {
		case NodeGroup:
		case NodeLayer:
			layer, err := parseV2Layer(m, base, rate)
			if err != nil {
				return nil, err
			}
			if _, ok := m["alphaMode"]; !ok {
				p.ConversionTraces = append(p.ConversionTraces, ConversionTrace{
					Path: base + ".alphaMode", OldValue: "<missing>",
					NewValue: string(AlphaPremultiplied), Rule: "schema-default",
					Detail: "v2 default alpha convention",
				})
			}
			node.LayerID = layer.ID
			layer.ParentID = parent
			node.Name = layer.Name
			p.Layers = append(p.Layers, layer)
			p.ConversionTraces = append(p.ConversionTraces, ConversionTrace{
				Path: base + ".alphaMode", OldValue: string(AlphaPremultiplied),
				NewValue: string(AlphaPremultiplied), Rule: "already-canonical",
			})
		default:
			return nil, schemaError(base+".kind", "expected group or layer")
		}
		p.Nodes = append(p.Nodes, node)
		if parent != "" {
			children[parent] = append(children[parent], nodeID)
		}
	}
	for i := range p.Nodes {
		p.Nodes[i].Children = children[p.Nodes[i].ID]
	}
	if err := p.ValidateHierarchy(); err != nil {
		return nil, err
	}
	if err := parseInstructions(obj, p, "instructions", "atTick", "op", "layerId", v2InstructionFields); err != nil {
		return nil, err
	}
	if err := parseFrames(obj, p, "frames", "timeTick"); err != nil {
		return nil, err
	}
	if err := parseTruth(obj, p); err != nil {
		return nil, err
	}
	return p, nil
}

func loadV3(obj map[string]any) (*Project, error) {
	p := &Project{SourceVersion: Version3, SourceFormat: "studio-tree-premul-tick", ParamVersion: 1}
	if err := parseCommonProjectFields(obj, p, "project-id", "frame-size", "window-size"); err != nil {
		return nil, err
	}
	tb, err := requiredObject(obj, "timebase", "timebase")
	if err != nil {
		return nil, err
	}
	unit, err := requiredString(tb, "unit", "timebase.unit")
	if err != nil {
		return nil, err
	}
	if unit != "tick" {
		return nil, schemaError("timebase.unit", "v3 requires tick")
	}
	rate, err := requiredInt(tb, "tick-rate", "timebase.tick-rate")
	if err != nil {
		return nil, err
	}
	rounding, err := requiredRounding(tb, "rounding", "timebase.rounding")
	if err != nil {
		return nil, err
	}
	p.Timebase = InternalTimebase(rate)
	p.Timebase.Rounding = rounding

	rawTree, err := requiredArray(obj, "tree", "tree")
	if err != nil {
		return nil, err
	}
	seen := make(map[string]bool)
	for i, raw := range rawTree {
		if err := parseV3Node(p, raw, "", fmt.Sprintf("tree[%d]", i), seen, rate); err != nil {
			return nil, err
		}
	}
	children := make(map[string][]string)
	for _, node := range p.Nodes {
		if node.ParentID != "" {
			children[node.ParentID] = append(children[node.ParentID], node.ID)
		}
	}
	for i := range p.Nodes {
		p.Nodes[i].Children = children[p.Nodes[i].ID]
	}
	if err := p.ValidateHierarchy(); err != nil {
		return nil, err
	}
	if err := parseInstructions(obj, p, "instructions", "at-tick", "op", "layer-id", v3InstructionFields); err != nil {
		return nil, err
	}
	if err := parseFrames(obj, p, "frames", "time-tick"); err != nil {
		return nil, err
	}
	if err := parseTruth(obj, p); err != nil {
		return nil, err
	}
	return p, nil
}

func parseCommonProjectFields(obj map[string]any, p *Project, idKey, frameKey, windowKey string) error {
	id, err := requiredString(obj, idKey, idKey)
	if err != nil {
		return err
	}
	p.ID = id
	frame, err := requiredObject(obj, frameKey, frameKey)
	if err != nil {
		return err
	}
	width, err := requiredInt(frame, "width", frameKey+".width")
	if err != nil {
		return err
	}
	height, err := requiredInt(frame, "height", frameKey+".height")
	if err != nil {
		return err
	}
	channels, err := requiredInt(frame, "channels", frameKey+".channels")
	if err != nil {
		return err
	}
	p.Width, p.Height, p.Channels = int(width), int(height), int(channels)
	window, err := requiredInt(obj, windowKey, windowKey)
	if err != nil {
		return err
	}
	p.WindowSize = int(window)
	return nil
}

func parseV2Layer(m map[string]any, base string, rate int64) (*Layer, error) {
	id, err := requiredString(m, "layerId", base+".layerId")
	if err != nil {
		return nil, err
	}
	name, _, err := optionalString(m, "name", base+".name")
	if err != nil {
		return nil, err
	}
	content, err := requiredString(m, "contentSha256", base+".contentSha256")
	if err != nil {
		return nil, err
	}
	opacity, err := requiredUint16(m, "opacity", base+".opacity")
	if err != nil {
		return nil, err
	}
	blend, err := requiredString(m, "blendMode", base+".blendMode")
	if err != nil {
		return nil, err
	}
	visible, err := requiredBool(m, "visible", base+".visible")
	if err != nil {
		return nil, err
	}
	end, err := requiredInt(m, "visibleUntilTick", base+".visibleUntilTick")
	if err != nil {
		return nil, err
	}
	alphaRaw, ok, err := optionalString(m, "alphaMode", base+".alphaMode")
	if err != nil {
		return nil, err
	}
	alpha := AlphaPremultiplied
	if ok {
		alpha = AlphaMode(alphaRaw)
	}
	if alpha != AlphaPremultiplied && alpha != AlphaStraight {
		return nil, schemaError(base+".alphaMode", "unsupported alpha mode")
	}
	layer := &Layer{
		ID: id, Name: name, ContentHash: content, Opacity: opacity,
		BlendMode: BlendMode(blend), Visible: visible, VisibleUntil: end,
		AlphaMode: AlphaPremultiplied, SourceAlphaMode: alpha, ParamVersion: 1,
	}
	if !layer.BlendMode.Valid() {
		return nil, schemaError(base+".blendMode", "unsupported blend mode "+blend)
	}
	if rate <= 0 {
		return nil, schemaError("timebase.tickRate", "must be positive")
	}
	return layer, nil
}

func parseV3Node(p *Project, raw any, parent, path string, seen map[string]bool, rate int64) error {
	m, err := asObject(raw, path)
	if err != nil {
		return err
	}
	id, err := requiredString(m, "node-id", path+".node-id")
	if err != nil {
		return err
	}
	if seen[id] {
		return schemaError(path+".node-id", "duplicate node id "+id)
	}
	seen[id] = true
	kind, err := requiredString(m, "kind", path+".kind")
	if err != nil {
		return err
	}
	declaredParent, _, err := optionalString(m, "parent-id", path+".parent-id")
	if err != nil {
		return err
	}
	if declaredParent != "" && declaredParent != parent {
		return schemaError(path+".parent-id", "nested parent does not match declared parent")
	}
	node := Node{ID: id, Kind: NodeKind(kind), ParentID: parent}
	switch node.Kind {
	case NodeGroup:
		name, _, err := optionalString(m, "name", path+".name")
		if err != nil {
			return err
		}
		node.Name = name
		p.Nodes = append(p.Nodes, node)
		children, err := requiredArray(m, "children", path+".children")
		if err != nil {
			return err
		}
		for i, child := range children {
			if err := parseV3Node(p, child, id, fmt.Sprintf("%s.children[%d]", path, i), seen, rate); err != nil {
				return err
			}
		}
		return nil
	case NodeLayer:
		layer, err := parseV3Layer(m, path, parent, rate)
		if err != nil {
			return err
		}
		node.Name = layer.Name
		node.LayerID = layer.ID
		p.Nodes = append(p.Nodes, node)
		p.Layers = append(p.Layers, layer)
		return nil
	default:
		return schemaError(path+".kind", "expected group or layer")
	}
}

func parseV3Layer(m map[string]any, path, parent string, rate int64) (*Layer, error) {
	id, err := requiredString(m, "layer-id", path+".layer-id")
	if err != nil {
		return nil, err
	}
	name, _, err := optionalString(m, "name", path+".name")
	if err != nil {
		return nil, err
	}
	content, err := requiredObject(m, "content", path+".content")
	if err != nil {
		return nil, err
	}
	digest, err := requiredString(content, "sha256", path+".content.sha256")
	if err != nil {
		return nil, err
	}
	opacity, err := requiredUint16(m, "opacity", path+".opacity")
	if err != nil {
		return nil, err
	}
	blend, err := requiredString(m, "blend-mode", path+".blend-mode")
	if err != nil {
		return nil, err
	}
	visibility, err := requiredObject(m, "visibility", path+".visibility")
	if err != nil {
		return nil, err
	}
	visible, err := requiredBool(visibility, "visible", path+".visibility.visible")
	if err != nil {
		return nil, err
	}
	end, err := requiredInt(visibility, "until-tick", path+".visibility.until-tick")
	if err != nil {
		return nil, err
	}
	alphaObj, err := requiredObject(m, "alpha", path+".alpha")
	if err != nil {
		return nil, err
	}
	alphaRaw, err := requiredString(alphaObj, "mode", path+".alpha.mode")
	if err != nil {
		return nil, err
	}
	alpha := AlphaMode(alphaRaw)
	if alpha != AlphaPremultiplied && alpha != AlphaStraight {
		return nil, schemaError(path+".alpha.mode", "unsupported alpha mode")
	}
	layer := &Layer{
		ID: id, Name: name, ParentID: parent, ContentHash: digest, Opacity: opacity,
		BlendMode: BlendMode(blend), Visible: visible, VisibleUntil: end,
		AlphaMode: AlphaPremultiplied, SourceAlphaMode: alpha, ParamVersion: 1,
	}
	if !layer.BlendMode.Valid() {
		return nil, schemaError(path+".blend-mode", "unsupported blend mode "+blend)
	}
	if rate <= 0 {
		return nil, schemaError("timebase.tick-rate", "must be positive")
	}
	return layer, nil
}

type instructionFields struct {
	sequence string
	action   string
	layer    string
	visible  string
	opacity  string
	end      string
	blend    string
}

var v1InstructionFields = instructionFields{
	sequence: "sequence", action: "action", layer: "layer_id",
	visible: "enabled", opacity: "opacity", end: "visible_until_ms", blend: "blend_mode",
}

var v2InstructionFields = instructionFields{
	sequence: "seq", action: "op", layer: "layerId",
	visible: "visible", opacity: "opacity", end: "visibleUntilTick", blend: "blendMode",
}

var v3InstructionFields = instructionFields{
	sequence: "seq", action: "op", layer: "layer-id",
	visible: "visible", opacity: "opacity", end: "until-tick", blend: "blend-mode",
}

func parseInstructions(obj map[string]any, p *Project, arrayKey, timeKey, actionKey, layerKey string, fields instructionFields) error {
	rawInstructions, err := requiredArray(obj, arrayKey, arrayKey)
	if err != nil {
		return err
	}
	rate := p.Timebase.TickRate
	rounding := p.Timebase.Rounding
	var previous int64 = -1
	for i, raw := range rawInstructions {
		base := fmt.Sprintf("%s[%d]", arrayKey, i)
		m, err := asObject(raw, base)
		if err != nil {
			return err
		}
		seq, err := requiredInt(m, fields.sequence, base+"."+fields.sequence)
		if err != nil {
			return err
		}
		at, err := requiredInt(m, timeKey, base+"."+timeKey)
		if err != nil {
			return err
		}
		if p.SourceVersion == Version1 {
			rawAt := at
			at, err = MillisecondsToTicks(at, rate, rounding, base+"."+timeKey)
			if err != nil {
				return err
			}
			p.ConversionTraces = append(p.ConversionTraces, ConversionTrace{
				Path: base + "." + timeKey, OldValue: strconv.FormatInt(rawAt, 10),
				NewValue: strconv.FormatInt(at, 10), Rule: "ms->tick",
				Detail: fmt.Sprintf("tick_rate=%d rounding=%s", rate, rounding),
			})
		}
		if at < previous {
			return schemaError(base+"."+timeKey, "instructions must be in logical time order")
		}
		previous = at
		actionRaw, err := requiredString(m, actionKey, base+"."+actionKey)
		if err != nil {
			return err
		}
		layerID, err := requiredString(m, layerKey, base+"."+layerKey)
		if err != nil {
			return err
		}
		if p.LayerByID(layerID) == nil {
			return schemaError(base+"."+layerKey, "unknown layer "+layerID)
		}
		action, err := normalizeAction(actionRaw)
		if err != nil {
			return schemaError(base+"."+actionKey, err.Error())
		}
		instruction := Instruction{Sequence: int(seq), At: at, Action: action, LayerID: layerID}
		switch action {
		case InstructionSetVisible:
			value, err := requiredBool(m, fields.visible, base+"."+fields.visible)
			if err != nil {
				return err
			}
			instruction.Visible = &value
		case InstructionSetOpacity:
			value, err := requiredUint16(m, fields.opacity, base+"."+fields.opacity)
			if err != nil {
				return err
			}
			instruction.Opacity = &value
		case InstructionSetVisibleUntil:
			value, err := requiredInt(m, fields.end, base+"."+fields.end)
			if err != nil {
				return err
			}
			if p.SourceVersion == Version1 {
				rawValue := value
				value, err = MillisecondsToTicks(value, rate, rounding, base+"."+fields.end)
				if err != nil {
					return err
				}
				p.ConversionTraces = append(p.ConversionTraces, ConversionTrace{
					Path: base + "." + fields.end, OldValue: strconv.FormatInt(rawValue, 10),
					NewValue: strconv.FormatInt(value, 10), Rule: "ms->tick",
					Detail: fmt.Sprintf("tick_rate=%d rounding=%s", rate, rounding),
				})
			}
			instruction.VisibleUntil = &value
		case InstructionSetBlendMode:
			value, err := requiredString(m, fields.blend, base+"."+fields.blend)
			if err != nil {
				return err
			}
			if !BlendMode(value).Valid() {
				return schemaError(base+"."+fields.blend, "unsupported blend mode")
			}
			instruction.BlendMode = BlendMode(value)
		}
		p.Instructions = append(p.Instructions, instruction)
	}
	return nil
}

func normalizeAction(raw string) (InstructionAction, error) {
	switch strings.ReplaceAll(strings.ToLower(raw), "-", "_") {
	case "set_visible", "setvisible":
		return InstructionSetVisible, nil
	case "set_opacity", "setopacity":
		return InstructionSetOpacity, nil
	case "set_visible_until", "setvisibleuntil":
		return InstructionSetVisibleUntil, nil
	case "set_blend_mode", "setblendmode":
		return InstructionSetBlendMode, nil
	default:
		return "", fmt.Errorf("unsupported instruction action %q", raw)
	}
}

func parseFrames(obj map[string]any, p *Project, arrayKey, timeKey string) error {
	rawFrames, err := requiredArray(obj, arrayKey, arrayKey)
	if err != nil {
		return err
	}
	for i, raw := range rawFrames {
		base := fmt.Sprintf("%s[%d]", arrayKey, i)
		m, err := asObject(raw, base)
		if err != nil {
			return err
		}
		index, err := requiredInt(m, "index", base+".index")
		if err != nil {
			return err
		}
		at, err := requiredInt(m, timeKey, base+"."+timeKey)
		if err != nil {
			return err
		}
		if p.SourceVersion == Version1 {
			rawAt := at
			at, err = MillisecondsToTicks(at, p.Timebase.TickRate, p.Timebase.Rounding, base+"."+timeKey)
			if err != nil {
				return err
			}
			p.ConversionTraces = append(p.ConversionTraces, ConversionTrace{
				Path: base + "." + timeKey, OldValue: strconv.FormatInt(rawAt, 10),
				NewValue: strconv.FormatInt(at, 10), Rule: "ms->tick",
				Detail: fmt.Sprintf("tick_rate=%d rounding=%s", p.Timebase.TickRate, p.Timebase.Rounding),
			})
		}
		rawPoints, err := requiredArray(m, "sample_points", base+".sample_points")
		if err != nil {
			return err
		}
		points := make([]Point, 0, len(rawPoints))
		for j, rawPoint := range rawPoints {
			pointBase := fmt.Sprintf("%s.sample_points[%d]", base, j)
			pointObj, err := asObject(rawPoint, pointBase)
			if err != nil {
				return err
			}
			x, err := requiredInt(pointObj, "x", pointBase+".x")
			if err != nil {
				return err
			}
			y, err := requiredInt(pointObj, "y", pointBase+".y")
			if err != nil {
				return err
			}
			points = append(points, Point{X: int(x), Y: int(y)})
		}
		p.Frames = append(p.Frames, FrameSpec{Index: int(index), TimeTicks: at, SamplePoints: points})
	}
	return nil
}

func parseTruth(obj map[string]any, p *Project) error {
	raw, ok := obj["truth"]
	if !ok {
		return nil
	}
	m, err := asObject(raw, "truth")
	if err != nil {
		return err
	}
	tolerance := int64(0)
	if rawTolerance, ok := m["tolerance"]; ok {
		tolerance, err = asInt(rawTolerance, "truth.tolerance")
		if err != nil {
			return err
		}
	}
	rawFrames, err := requiredArray(m, "frames", "truth.frames")
	if err != nil {
		return err
	}
	truth := &Truth{Tolerance: int(tolerance)}
	for i, rawFrame := range rawFrames {
		base := fmt.Sprintf("truth.frames[%d]", i)
		fm, err := asObject(rawFrame, base)
		if err != nil {
			return err
		}
		index, err := requiredInt(fm, "index", base+".index")
		if err != nil {
			return err
		}
		at, err := requiredInt(fm, "timeTick", base+".timeTick")
		if err != nil {
			return err
		}
		endsObj, err := requiredObject(fm, "layerEnds", base+".layerEnds")
		if err != nil {
			return err
		}
		ends := make(map[string]int64, len(endsObj))
		keys := make([]string, 0, len(endsObj))
		for key := range endsObj {
			keys = append(keys, key)
		}
		sort.Strings(keys)
		for _, key := range keys {
			value, err := asInt(endsObj[key], base+".layerEnds."+key)
			if err != nil {
				return err
			}
			ends[key] = value
		}
		rawPixels, err := requiredArray(fm, "samplePixels", base+".samplePixels")
		if err != nil {
			return err
		}
		pixels := make([]TruthPixel, 0, len(rawPixels))
		for j, rawPixel := range rawPixels {
			pixelBase := fmt.Sprintf("%s.samplePixels[%d]", base, j)
			pm, err := asObject(rawPixel, pixelBase)
			if err != nil {
				return err
			}
			x, err := requiredInt(pm, "x", pixelBase+".x")
			if err != nil {
				return err
			}
			y, err := requiredInt(pm, "y", pixelBase+".y")
			if err != nil {
				return err
			}
			rawRGBA, err := requiredArray(pm, "rgba", pixelBase+".rgba")
			if err != nil {
				return err
			}
			if len(rawRGBA) != 4 {
				return schemaError(pixelBase+".rgba", "expected four channels")
			}
			var rgba [4]uint16
			for k, rawChannel := range rawRGBA {
				value, err := asInt(rawChannel, fmt.Sprintf("%s.rgba[%d]", pixelBase, k))
				if err != nil {
					return err
				}
				if value < 0 || value > 65535 {
					return schemaError(fmt.Sprintf("%s.rgba[%d]", pixelBase, k), "outside uint16")
				}
				rgba[k] = uint16(value)
			}
			pixels = append(pixels, TruthPixel{X: int(x), Y: int(y), RGBA: rgba})
		}
		truth.Frames = append(truth.Frames, FrameTruth{
			Index: int(index), TimeTicks: at, LayerEnds: ends, SamplePixels: pixels,
		})
	}
	p.Truth = truth
	return nil
}

func asObject(value any, path string) (map[string]any, error) {
	obj, ok := value.(map[string]any)
	if !ok {
		return nil, schemaError(path, "expected object")
	}
	return obj, nil
}

func asArray(value any, path string) ([]any, error) {
	arr, ok := value.([]any)
	if !ok {
		return nil, schemaError(path, "expected array")
	}
	return arr, nil
}

func asInt(value any, path string) (int64, error) {
	number, ok := value.(json.Number)
	if !ok {
		if f, ok := value.(float64); ok {
			if f != float64(int64(f)) {
				return 0, schemaError(path, "expected integer")
			}
			return int64(f), nil
		}
		return 0, schemaError(path, "expected integer")
	}
	result, err := number.Int64()
	if err != nil {
		return 0, schemaError(path, "expected 64-bit integer")
	}
	return result, nil
}

func requiredValue(obj map[string]any, key, path string) (any, error) {
	value, ok := obj[key]
	if !ok {
		return nil, schemaError(path, "required field is missing")
	}
	if value == nil {
		return nil, schemaError(path, "required field is null")
	}
	return value, nil
}

func requiredObject(obj map[string]any, key, path string) (map[string]any, error) {
	value, err := requiredValue(obj, key, path)
	if err != nil {
		return nil, err
	}
	return asObject(value, path)
}

func requiredArray(obj map[string]any, key, path string) ([]any, error) {
	value, err := requiredValue(obj, key, path)
	if err != nil {
		return nil, err
	}
	return asArray(value, path)
}

func requiredString(obj map[string]any, key, path string) (string, error) {
	value, err := requiredValue(obj, key, path)
	if err != nil {
		return "", err
	}
	str, ok := value.(string)
	if !ok {
		return "", schemaError(path, "expected string")
	}
	return str, nil
}

func optionalString(obj map[string]any, key, path string) (string, bool, error) {
	value, ok := obj[key]
	if !ok || value == nil {
		return "", false, nil
	}
	str, ok := value.(string)
	if !ok {
		return "", false, schemaError(path, "expected string")
	}
	return str, true, nil
}

func requiredInt(obj map[string]any, key, path string) (int64, error) {
	value, err := requiredValue(obj, key, path)
	if err != nil {
		return 0, err
	}
	return asInt(value, path)
}

func requiredUint16(obj map[string]any, key, path string) (uint16, error) {
	value, err := requiredInt(obj, key, path)
	if err != nil {
		return 0, err
	}
	if value < 0 || value > 65535 {
		return 0, schemaError(path, "outside uint16")
	}
	return uint16(value), nil
}

func requiredBool(obj map[string]any, key, path string) (bool, error) {
	value, err := requiredValue(obj, key, path)
	if err != nil {
		return false, err
	}
	b, ok := value.(bool)
	if !ok {
		return false, schemaError(path, "expected boolean")
	}
	return b, nil
}

func requiredRounding(obj map[string]any, key, path string) (Rounding, error) {
	raw, err := requiredString(obj, key, path)
	if err != nil {
		return "", err
	}
	rounding := Rounding(raw)
	if !rounding.Valid() {
		return "", schemaError(path, "unsupported rounding rule "+raw)
	}
	return rounding, nil
}
