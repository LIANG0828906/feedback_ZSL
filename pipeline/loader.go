package pipeline

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
)

const (
	BundleFormat      = "virtual-stage-bundle/v1"
	InstructionFormat = "virtual-stage-instructions/v1"
	V1Format          = "foreground-composite/v1"
	V2Format          = "foreground-composite/v2"
	V3Format          = "foreground-composite/v3"
)

type manifest struct {
	Format       string   `json:"format"`
	Name         string   `json:"name"`
	Seed         int64    `json:"seed"`
	Sources      []string `json:"sources"`
	Instructions string   `json:"instructions"`
	Truth        string   `json:"truth"`
	WindowSize   int      `json:"window_size"`
}

// LoadProject loads a deterministic bundle, its ordered layer sources and
// per-frame instructions. Paths in the manifest are relative to the manifest.
func LoadProject(manifestPath string) (*Model, *manifest, error) {
	raw, err := os.ReadFile(manifestPath)
	if err != nil {
		return nil, nil, errCode(CodeDecode, "read project manifest: %v", err)
	}
	var mf manifest
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&mf); err != nil {
		return nil, nil, errCode(CodeDecode, "decode project manifest: %v", err)
	}
	if mf.Format != BundleFormat {
		return nil, nil, versionError("$.format", "format", "unsupported bundle format %q", mf.Format)
	}
	if len(mf.Sources) == 0 {
		return nil, nil, versionError("$.sources", "sources", "bundle has no layer sources")
	}
	base := filepath.Dir(manifestPath)
	paths := make([]string, 0, len(mf.Sources))
	for _, src := range mf.Sources {
		if !filepath.IsAbs(src) {
			src = filepath.Join(base, src)
		}
		paths = append(paths, filepath.Clean(src))
	}
	model, err := LoadFiles(paths)
	if err != nil {
		return nil, nil, err
	}
	model.Name = mf.Name
	if mf.Instructions != "" {
		insPath := mf.Instructions
		if !filepath.IsAbs(insPath) {
			insPath = filepath.Join(base, insPath)
		}
		frameCount, instructions, err := LoadInstructions(insPath)
		if err != nil {
			return nil, nil, err
		}
		model.FrameCount = frameCount
		model.Instructions = instructions
	}
	if model.FrameCount <= 0 {
		return nil, nil, versionError("$", "frame_count", "frame_count could not be derived from the project")
	}
	model.Normalize()
	if err := model.Validate(); err != nil {
		return nil, nil, err
	}
	return model, &mf, nil
}

// LoadFiles reads one or more versioned layer documents and merges them into a
// single canonical model. Source document order defines the bottom-to-top stack
// when each document contributes layers that do not declare a shared stack.
func LoadFiles(paths []string) (*Model, error) {
	if len(paths) == 0 {
		return nil, versionError("$", "sources", "at least one layer source is required")
	}
	var merged *Model
	var allSourceVersions []int
	for _, path := range paths {
		part, err := LoadFile(path)
		if err != nil {
			return nil, err
		}
		part.Normalize()
		if merged == nil {
			merged = &Model{
				Name:            part.Name,
				Canvas:          part.Canvas,
				TimeBase:        part.TimeBase,
				SourceTimeBases: append([]TimeBase(nil), part.SourceTimeBases...),
				FrameCount:      part.FrameCount,
			}
			allSourceVersions = append(allSourceVersions, part.SourceVersions...)
		} else {
			if err := compatibleModels(merged, part); err != nil {
				return nil, err
			}
			allSourceVersions = append(allSourceVersions, part.SourceVersions...)
			merged.SourceTimeBases = append(merged.SourceTimeBases, part.SourceTimeBases...)
			if part.FrameCount > merged.FrameCount {
				merged.FrameCount = part.FrameCount
			}
		}
		seen := map[string]bool{}
		for _, layer := range merged.Layers {
			seen[layer.ID] = true
		}
		for _, layer := range part.Layers {
			if seen[layer.ID] {
				return nil, layerError(layer.ID, "duplicate layer id while merging sources")
			}
			merged.Layers = append(merged.Layers, layer)
		}
		merged.Stack = append(merged.Stack, part.Stack...)
		format := ""
		if len(part.Sources) > 0 {
			format = part.Sources[0].Format
		}
		merged.Sources = append(merged.Sources, SourceRef{Path: path, Version: part.SourceVersion, Format: format})
	}
	merged.SourceVersions = allSourceVersions
	merged.Normalize()
	if err := merged.Validate(); err != nil {
		return nil, err
	}
	return merged, nil
}

func compatibleModels(a, b *Model) error {
	if a.Canvas != b.Canvas {
		return paraError(-1, "source canvas mismatch: %+v vs %+v", a.Canvas, b.Canvas)
	}
	if !a.TimeBase.Seconds().Equal(b.TimeBase.Seconds()) {
		return paraError(-1, "source time base mismatch: %g vs %g", a.TimeBase.FramePeriod.Float64(), b.TimeBase.FramePeriod.Float64())
	}
	return nil
}

// LoadFile detects and decodes one source document. Malformed JSON is E_DECODE,
// an unknown/missing format version is E_VER, and wrong field types are
// E_SCHEMA with an object path and field name.
func LoadFile(path string) (*Model, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil, errCode(CodeDecode, "read %s: %v", path, err)
	}
	root, err := decodeObject(raw, "$")
	if err != nil {
		return nil, err
	}
	format, err := reqString(root, "format", "$")
	if err != nil {
		if e, ok := err.(*Error); ok && e.Code == CodeSchema {
			return nil, err
		}
		return nil, versionError("$.format", "format", "required format version is missing")
	}
	var model *Model
	switch format {
	case V1Format:
		model, err = decodeV1(root)
	case V2Format:
		model, err = decodeV2(root)
	case V3Format:
		model, err = decodeV3(root)
	default:
		return nil, versionError("$.format", "format", "unknown foreground composite version %q", format)
	}
	if err != nil {
		return nil, err
	}
	for i := range model.Sources {
		if model.Sources[i].Path == "" {
			model.Sources[i].Path = path
		}
	}
	model.Normalize()
	if err := model.Validate(); err != nil {
		return nil, err
	}
	return model, nil
}

func LoadInstructions(path string) (int, []FrameInstruction, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return 0, nil, errCode(CodeDecode, "read instructions: %v", err)
	}
	root, err := decodeObject(raw, "$")
	if err != nil {
		return 0, nil, err
	}
	format, err := reqString(root, "format", "$")
	if err != nil {
		return 0, nil, versionError("$.format", "format", "required instruction format is missing")
	}
	if format != InstructionFormat {
		return 0, nil, versionError("$.format", "format", "unknown instruction format %q", format)
	}
	frameCount, err := reqInt(root, "frame_count", "$")
	if err != nil {
		return 0, nil, err
	}
	if frameCount <= 0 {
		return 0, nil, paraError(-1, "instruction frame_count must be positive")
	}
	rawFrames, _, err := reqArray(root, "frames", "$")
	if err != nil {
		return 0, nil, err
	}
	instructions := make([]FrameInstruction, 0, len(rawFrames))
	for i, rawFrame := range rawFrames {
		path := fmt.Sprintf("$.frames[%d]", i)
		frameObj, err := asObject(rawFrame, path)
		if err != nil {
			return 0, nil, err
		}
		frame, err := reqInt(frameObj, "frame", path)
		if err != nil {
			return 0, nil, err
		}
		if frame < 0 || frame >= frameCount {
			return 0, nil, paraError(-1, "instruction frame %d is outside [0,%d)", frame, frameCount)
		}
		ins := FrameInstruction{Frame: frame, Layers: map[string]LayerOverride{}}
		rawLayers, _, err := reqObject(frameObj, "layers", path)
		if err != nil {
			return 0, nil, err
		}
		for id, rawOverride := range rawLayers {
			lp := path + ".layers." + id
			ovObj, err := asObject(rawOverride, lp)
			if err != nil {
				return 0, nil, err
			}
			var ov LayerOverride
			if _, ok := ovObj["visible"]; ok {
				v, err := reqBool(ovObj, "visible", lp)
				if err != nil {
					return 0, nil, err
				}
				ov.Visible = &v
			}
			if _, ok := ovObj["opacity"]; ok {
				v, err := reqFloat(ovObj, "opacity", lp)
				if err != nil {
					return 0, nil, err
				}
				if v < 0 || v > 1 {
					return 0, nil, paraError(-1, "instruction opacity for %s is outside [0,1]", id)
				}
				ov.Opacity = &v
			}
			if _, ok := ovObj["alpha"]; ok {
				v, err := reqFloat(ovObj, "alpha", lp)
				if err != nil {
					return 0, nil, err
				}
				if v < 0 || v > 1 {
					return 0, nil, paraError(-1, "instruction alpha for %s is outside [0,1]", id)
				}
				ov.Alpha = &v
			}
			if _, ok := ovObj["color"]; ok {
				arr, _, err := reqArray(ovObj, "color", lp)
				if err != nil {
					return 0, nil, err
				}
				col, err := floatTriplet(arr, lp+".color")
				if err != nil {
					return 0, nil, err
				}
				ov.Color = &col
			}
			ins.Layers[id] = ov
		}
		instructions = append(instructions, ins)
	}
	sort.SliceStable(instructions, func(i, j int) bool { return instructions[i].Frame < instructions[j].Frame })
	for i := 1; i < len(instructions); i++ {
		if instructions[i].Frame == instructions[i-1].Frame {
			return 0, nil, paraError(-1, "duplicate instruction for frame %d", instructions[i].Frame)
		}
	}
	return frameCount, instructions, nil
}

func decodeV1(root map[string]any) (*Model, error) {
	model := &Model{SourceVersion: 1, SourceVersions: []int{1}, Canvas: Canvas{ColorSpace: ColorLinear}}
	scene, scenePath, err := reqObject(root, "scene", "$")
	if err != nil {
		return nil, err
	}
	model.Name, err = reqString(scene, "name", scenePath)
	if err != nil {
		return nil, err
	}
	model.Canvas.Width, err = reqInt(scene, "width", scenePath)
	if err != nil {
		return nil, err
	}
	model.Canvas.Height, err = reqInt(scene, "height", scenePath)
	if err != nil {
		return nil, err
	}
	model.Canvas.Channels, err = reqInt(scene, "channels", scenePath)
	if err != nil {
		return nil, err
	}
	colorSpace, err := reqString(scene, "color_space", scenePath)
	if err != nil {
		return nil, err
	}
	model.Canvas.ColorSpace = colorSpace
	fpsNum, err := reqInt64(scene, "frame_rate_num", scenePath)
	if err != nil {
		return nil, err
	}
	fpsDen, err := reqInt64(scene, "frame_rate_den", scenePath)
	if err != nil {
		return nil, err
	}
	if fpsNum <= 0 || fpsDen <= 0 {
		return nil, paraError(-1, "v1 frame rate must be positive")
	}
	model.TimeBase = TimeBase{Unit: "second", FramePeriod: NewRational(fpsDen, fpsNum)}
	model.SourceTimeBases = []TimeBase{model.TimeBase}
	premul, err := reqBool(scene, "premultiplied", scenePath)
	if err != nil {
		return nil, err
	}
	layers, _, err := reqArray(root, "layers", "$")
	if err != nil {
		return nil, err
	}
	maxOut := 0
	for i, raw := range layers {
		path := fmt.Sprintf("$.layers[%d]", i)
		obj, err := asObject(raw, path)
		if err != nil {
			return nil, err
		}
		id, err := reqString(obj, "id", path)
		if err != nil {
			return nil, err
		}
		name, err := reqString(obj, "name", path)
		if err != nil {
			return nil, err
		}
		kind, err := reqString(obj, "kind", path)
		if err != nil {
			return nil, err
		}
		enabled, err := reqBool(obj, "enabled", path)
		if err != nil {
			return nil, err
		}
		opacity, err := reqFloat(obj, "opacity", path)
		if err != nil {
			return nil, err
		}
		color, err := reqColor(obj, "color", path)
		if err != nil {
			return nil, err
		}
		alpha, err := reqFloat(obj, "alpha", path)
		if err != nil {
			return nil, err
		}
		inFrame, err := reqInt(obj, "in_frame", path)
		if err != nil {
			return nil, err
		}
		outFrame, err := reqInt(obj, "out_frame", path)
		if err != nil {
			return nil, err
		}
		if outFrame > maxOut {
			maxOut = outFrame
		}
		shape, err := decodeShape(obj, "shape", path)
		if err != nil {
			return nil, err
		}
		encoding := ColorSRGB
		if rawEnc, ok := obj["color_encoding"]; ok {
			encoding, err = asString(rawEnc, path+".color_encoding")
			if err != nil {
				return nil, err
			}
		}
		parent := ""
		if rawParent, ok := obj["parent_id"]; ok && rawParent != nil {
			parent, err = asString(rawParent, path+".parent_id")
			if err != nil {
				return nil, err
			}
		}
		alphaMode := AlphaStraight
		if premul {
			alphaMode = AlphaPremultiplied
		}
		model.Layers = append(model.Layers, Layer{
			ID: id, Name: name, Kind: kind, ParentID: parent, Visible: enabled,
			InFrame: inFrame, OutFrame: outFrame, VisibleUntilFrame: outFrame,
			Opacity: opacity, Alpha: alpha, Color: color, ColorEncoding: encoding,
			AlphaMode: alphaMode, Shape: shape, SourceVersion: 1,
		})
	}
	order, _, err := reqArray(root, "layer_order", "$")
	if err != nil {
		return nil, err
	}
	for i, raw := range order {
		id, err := asString(raw, fmt.Sprintf("$.layer_order[%d]", i))
		if err != nil {
			return nil, err
		}
		model.Stack = append(model.Stack, id)
	}
	model.FrameCount = maxOut
	if rawFrameCount, ok := root["frame_count"]; ok {
		frameCount, err := asInt(rawFrameCount, "$.frame_count")
		if err != nil {
			return nil, err
		}
		if frameCount > 0 {
			model.FrameCount = frameCount
		}
	}
	model.Sources = []SourceRef{{Version: 1, Format: V1Format}}
	return model, nil
}

func decodeV2(root map[string]any) (*Model, error) {
	model := &Model{SourceVersion: 2, SourceVersions: []int{2}, Canvas: Canvas{ColorSpace: ColorLinear}}
	spec, err := reqString(root, "spec_version", "$")
	if err != nil {
		return nil, err
	}
	if spec != "2.0" && spec != "2" {
		return nil, versionError("$.spec_version", "spec_version", "unknown v2 spec version %q", spec)
	}
	canvas, canvasPath, err := reqObject(root, "canvas", "$")
	if err != nil {
		return nil, err
	}
	model.Canvas.Width, err = reqInt(canvas, "width", canvasPath)
	if err != nil {
		return nil, err
	}
	model.Canvas.Height, err = reqInt(canvas, "height", canvasPath)
	if err != nil {
		return nil, err
	}
	model.Canvas.Channels, err = reqInt(canvas, "channels", canvasPath)
	if err != nil {
		return nil, err
	}
	model.Canvas.ColorSpace, err = reqString(root, "working_color_space", "$")
	if err != nil {
		return nil, err
	}
	alpha, alphaPath, err := reqObject(root, "alpha", "$")
	if err != nil {
		return nil, err
	}
	alphaMode, err := reqString(alpha, "mode", alphaPath)
	if err != nil {
		return nil, err
	}
	if alphaMode != AlphaPremultiplied && alphaMode != AlphaStraight {
		return nil, paraError(-1, "v2 alpha mode %q is unsupported", alphaMode)
	}
	timeObj, timePath, err := reqObject(root, "time", "$")
	if err != nil {
		return nil, err
	}
	rate, ratePath, err := reqObject(timeObj, "frame_rate", timePath)
	if err != nil {
		return nil, err
	}
	fpsNum, err := reqInt64(rate, "numerator", ratePath)
	if err != nil {
		return nil, err
	}
	fpsDen, err := reqInt64(rate, "denominator", ratePath)
	if err != nil {
		return nil, err
	}
	unit, err := reqString(timeObj, "unit", timePath)
	if err != nil {
		return nil, err
	}
	if unit != "second" && unit != "seconds" {
		return nil, paraError(-1, "v2 time unit %q is unsupported", unit)
	}
	if fpsNum <= 0 || fpsDen <= 0 {
		return nil, paraError(-1, "v2 frame rate must be positive")
	}
	model.TimeBase = TimeBase{Unit: "second", FramePeriod: NewRational(fpsDen, fpsNum)}
	model.SourceTimeBases = []TimeBase{model.TimeBase}
	rawLayers, _, err := reqArray(root, "layers", "$")
	if err != nil {
		return nil, err
	}
	maxOut := 0
	for i, raw := range rawLayers {
		path := fmt.Sprintf("$.layers[%d]", i)
		obj, err := asObject(raw, path)
		if err != nil {
			return nil, err
		}
		id, err := reqString(obj, "uid", path)
		if err != nil {
			return nil, err
		}
		name, err := reqString(obj, "display_name", path)
		if err != nil {
			return nil, err
		}
		kind, err := reqString(obj, "layer_type", path)
		if err != nil {
			return nil, err
		}
		visibility, visPath, err := reqObject(obj, "visibility", path)
		if err != nil {
			return nil, err
		}
		visible, err := reqBool(visibility, "visible", visPath)
		if err != nil {
			return nil, err
		}
		start, err := reqInt(visibility, "start_frame", visPath)
		if err != nil {
			return nil, err
		}
		until, err := reqInt(visibility, "until_frame", visPath)
		if err != nil {
			return nil, err
		}
		if until > maxOut {
			maxOut = until
		}
		hierarchy, hierPath, err := reqObject(obj, "hierarchy", path)
		if err != nil {
			return nil, err
		}
		parent := ""
		if rawParent, ok := hierarchy["parent"]; ok && rawParent != nil {
			parent, err = asString(rawParent, hierPath+".parent")
			if err != nil {
				return nil, err
			}
		}
		pixel, pixelPath, err := reqObject(obj, "pixel", path)
		if err != nil {
			return nil, err
		}
		color, err := reqColor(pixel, "color", pixelPath)
		if err != nil {
			return nil, err
		}
		opacity, err := reqFloat(pixel, "opacity", pixelPath)
		if err != nil {
			return nil, err
		}
		layerAlpha, err := reqFloat(pixel, "alpha", pixelPath)
		if err != nil {
			return nil, err
		}
		encoding, err := reqString(pixel, "encoding", pixelPath)
		if err != nil {
			return nil, err
		}
		bounds, boundsPath, err := reqObject(obj, "bounds", path)
		if err != nil {
			return nil, err
		}
		shapeType, err := reqString(bounds, "kind", boundsPath)
		if err != nil {
			return nil, err
		}
		shape := Shape{Type: shapeType}
		if shapeType == ShapeRect || shapeType == ShapeEllipse {
			rawRect, _, err := reqArray(bounds, "rect", boundsPath)
			if err != nil {
				return nil, err
			}
			rect, err := floatQuad(rawRect, boundsPath+".rect")
			if err != nil {
				return nil, err
			}
			shape.Rect = rect
		}
		model.Layers = append(model.Layers, Layer{
			ID: id, Name: name, Kind: kind, ParentID: parent, Visible: visible,
			InFrame: start, OutFrame: until, VisibleUntilFrame: until,
			Opacity: opacity, Alpha: layerAlpha, Color: color, ColorEncoding: encoding,
			AlphaMode: alphaMode, Shape: shape, SourceVersion: 2,
		})
	}
	rawStack, _, err := reqArray(root, "stack", "$")
	if err != nil {
		return nil, err
	}
	for i, raw := range rawStack {
		id, err := asString(raw, fmt.Sprintf("$.stack[%d]", i))
		if err != nil {
			return nil, err
		}
		model.Stack = append(model.Stack, id)
	}
	model.FrameCount = maxOut
	if rawFrameCount, ok := root["frame_count"]; ok {
		frameCount, err := asInt(rawFrameCount, "$.frame_count")
		if err != nil {
			return nil, err
		}
		if frameCount > 0 {
			model.FrameCount = frameCount
		}
	}
	model.Sources = []SourceRef{{Version: 2, Format: V2Format}}
	return model, nil
}

func decodeV3(root map[string]any) (*Model, error) {
	model := &Model{SourceVersion: 3, SourceVersions: []int{3}, Canvas: Canvas{ColorSpace: ColorLinear}}
	schemaVersion, err := reqInt(root, "schema_version", "$")
	if err != nil {
		return nil, err
	}
	if schemaVersion != 3 {
		return nil, versionError("$.schema_version", "schema_version", "unknown FC3 schema version %d", schemaVersion)
	}
	canvas, canvasPath, err := reqObject(root, "canvas", "$")
	if err != nil {
		return nil, err
	}
	model.Canvas.Width, err = reqInt(canvas, "w", canvasPath)
	if err != nil {
		return nil, err
	}
	model.Canvas.Height, err = reqInt(canvas, "h", canvasPath)
	if err != nil {
		return nil, err
	}
	model.Canvas.Channels, err = reqInt(canvas, "channels", canvasPath)
	if err != nil {
		return nil, err
	}
	color, colorPath, err := reqObject(root, "color", "$")
	if err != nil {
		return nil, err
	}
	model.Canvas.ColorSpace, err = reqString(color, "space", colorPath)
	if err != nil {
		return nil, err
	}
	defaultAlpha, err := reqString(color, "alpha_mode", colorPath)
	if err != nil {
		return nil, err
	}
	perLayerAlpha := false
	if raw, ok := color["per_layer_alpha"]; ok {
		perLayerAlpha, err = asBool(raw, colorPath+".per_layer_alpha")
		if err != nil {
			return nil, err
		}
	}
	timing, timingPath, err := reqObject(root, "timing", "$")
	if err != nil {
		return nil, err
	}
	tickRate, err := reqInt64(timing, "tick_rate", timingPath)
	if err != nil {
		return nil, err
	}
	ticksPerFrame, err := reqInt64(timing, "ticks_per_frame", timingPath)
	if err != nil {
		return nil, err
	}
	unit, err := reqString(timing, "unit", timingPath)
	if err != nil {
		return nil, err
	}
	if tickRate <= 0 || ticksPerFrame <= 0 {
		return nil, paraError(-1, "v3 tick timing must be positive")
	}
	if unit != "tick" && unit != "ticks" {
		return nil, paraError(-1, "v3 time unit %q is unsupported", unit)
	}
	model.TimeBase = TimeBase{Unit: "tick", FramePeriod: NewRational(ticksPerFrame, tickRate), TicksPerFrame: ticksPerFrame, TickRate: tickRate}
	model.SourceTimeBases = []TimeBase{model.TimeBase}
	rawLayers, _, err := reqArray(root, "layers", "$")
	if err != nil {
		return nil, err
	}
	maxOut := 0
	for i, raw := range rawLayers {
		path := fmt.Sprintf("$.layers[%d]", i)
		obj, err := asObject(raw, path)
		if err != nil {
			return nil, err
		}
		id, err := reqString(obj, "layer_key", path)
		if err != nil {
			return nil, err
		}
		name, err := reqString(obj, "title", path)
		if err != nil {
			return nil, err
		}
		kind, err := reqString(obj, "role", path)
		if err != nil {
			return nil, err
		}
		visible, err := reqBool(obj, "visible", path)
		if err != nil {
			return nil, err
		}
		lifecycle, lifePath, err := reqObject(obj, "lifecycle", path)
		if err != nil {
			return nil, err
		}
		start, err := reqInt(lifecycle, "start", lifePath)
		if err != nil {
			return nil, err
		}
		end, err := reqInt(lifecycle, "end", lifePath)
		if err != nil {
			return nil, err
		}
		if end > maxOut {
			maxOut = end
		}
		parent := ""
		if rawParent, ok := obj["parent"]; ok && rawParent != nil {
			parent, err = asString(rawParent, path+".parent")
			if err != nil {
				return nil, err
			}
		}
		source, srcPath, err := reqObject(obj, "source", path)
		if err != nil {
			return nil, err
		}
		colorValue, err := reqColor(source, "color", srcPath)
		if err != nil {
			return nil, err
		}
		encoding, err := reqString(source, "encoding", srcPath)
		if err != nil {
			return nil, err
		}
		layerAlpha, err := reqFloat(source, "alpha", srcPath)
		if err != nil {
			return nil, err
		}
		alphaMode := defaultAlpha
		if perLayerAlpha {
			alphaMode, err = v3AlphaMode(source, "premultiplied", srcPath, id)
			if err != nil {
				return nil, err
			}
		}
		blend, blendPath, err := reqObject(obj, "blend", path)
		if err != nil {
			return nil, err
		}
		opacity, err := reqFloat(blend, "opacity", blendPath)
		if err != nil {
			return nil, err
		}
		mask, maskPath, err := reqObject(obj, "mask", path)
		if err != nil {
			return nil, err
		}
		shapeType, err := reqString(mask, "type", maskPath)
		if err != nil {
			return nil, err
		}
		shape := Shape{Type: shapeType}
		if shapeType == ShapeRect || shapeType == ShapeEllipse {
			rawRect, _, err := reqArray(mask, "rect", maskPath)
			if err != nil {
				return nil, err
			}
			rect, err := floatQuad(rawRect, maskPath+".rect")
			if err != nil {
				return nil, err
			}
			shape.Rect = rect
		}
		model.Layers = append(model.Layers, Layer{
			ID: id, Name: name, Kind: kind, ParentID: parent, Visible: visible,
			InFrame: start, OutFrame: end, VisibleUntilFrame: end,
			Opacity: opacity, Alpha: layerAlpha, Color: colorValue, ColorEncoding: encoding,
			AlphaMode: alphaMode, Shape: shape, SourceVersion: 3,
		})
	}
	hierarchy, hierPath, err := reqObject(root, "hierarchy", "$")
	if err != nil {
		return nil, err
	}
	rawStack, _, err := reqArray(hierarchy, "render_order", hierPath)
	if err != nil {
		return nil, err
	}
	for i, raw := range rawStack {
		id, err := asString(raw, fmt.Sprintf("%s.render_order[%d]", hierPath, i))
		if err != nil {
			return nil, err
		}
		model.Stack = append(model.Stack, id)
	}
	model.FrameCount = maxOut
	if rawFrameCount, ok := root["frame_count"]; ok {
		frameCount, err := asInt(rawFrameCount, "$.frame_count")
		if err != nil {
			return nil, err
		}
		if frameCount > 0 {
			model.FrameCount = frameCount
		}
	}
	model.Sources = []SourceRef{{Version: 3, Format: V3Format}}
	return model, nil
}

func v3AlphaMode(obj map[string]any, key, parentPath, layerID string) (string, error) {
	path := joinPath(parentPath, key)
	raw, ok := obj[key]
	if !ok || raw == nil {
		return "", versionError(path, key, "required field is missing")
	}
	if value, ok := raw.(bool); ok {
		if value {
			return AlphaPremultiplied, nil
		}
		return AlphaStraight, nil
	}
	value, err := asString(raw, path)
	if err != nil {
		return "", err
	}
	switch value {
	case "true", "premultiplied", "premul":
		return AlphaPremultiplied, nil
	case "false", "straight", "non-premultiplied", "unpremultiplied":
		return AlphaStraight, nil
	default:
		return "", paraError(-1, "v3 layer %s has invalid alpha mode %q", layerID, value)
	}
}

func decodeShape(obj map[string]any, key, parentPath string) (Shape, error) {
	path := parentPath + "." + key
	shapeObj, _, err := reqObject(obj, key, parentPath)
	if err != nil {
		return Shape{}, err
	}
	shapeType, err := reqString(shapeObj, "type", path)
	if err != nil {
		return Shape{}, err
	}
	shape := Shape{Type: shapeType}
	if shapeType == ShapeRect || shapeType == ShapeEllipse {
		rawRect, _, err := reqArray(shapeObj, "rect", path)
		if err != nil {
			return Shape{}, err
		}
		rect, err := floatQuad(rawRect, path+".rect")
		if err != nil {
			return Shape{}, err
		}
		shape.Rect = rect
	}
	return shape, nil
}

func decodeObject(raw []byte, path string) (map[string]any, error) {
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.UseNumber()
	var v any
	if err := dec.Decode(&v); err != nil {
		return nil, errCode(CodeDecode, "decode JSON at %s: %v", path, err)
	}
	var trailing any
	if err := dec.Decode(&trailing); err != io.EOF {
		return nil, errCode(CodeDecode, "decode JSON at %s: trailing data", path)
	}
	return asObject(v, path)
}

func asObject(v any, path string) (map[string]any, error) {
	obj, ok := v.(map[string]any)
	if !ok {
		return nil, schemaError(path, lastField(path), "expected object, got %s", jsonType(v))
	}
	return obj, nil
}

func asString(v any, path string) (string, error) {
	s, ok := v.(string)
	if !ok {
		return "", schemaError(path, lastField(path), "expected string, got %s", jsonType(v))
	}
	return s, nil
}

func asBool(v any, path string) (bool, error) {
	b, ok := v.(bool)
	if !ok {
		return false, schemaError(path, lastField(path), "expected boolean, got %s", jsonType(v))
	}
	return b, nil
}

func asNumber(v any, path string) (float64, error) {
	n, ok := v.(json.Number)
	if !ok {
		return 0, schemaError(path, lastField(path), "expected number, got %s", jsonType(v))
	}
	f, err := n.Float64()
	if err != nil {
		return 0, schemaError(path, lastField(path), "invalid number %q", n)
	}
	return f, nil
}

func asInt(v any, path string) (int, error) {
	n, ok := v.(json.Number)
	if !ok {
		return 0, schemaError(path, lastField(path), "expected integer, got %s", jsonType(v))
	}
	i, err := strconv.ParseInt(n.String(), 10, 64)
	if err != nil {
		return 0, schemaError(path, lastField(path), "expected integer, got %q", n)
	}
	return int(i), nil
}

func reqObject(obj map[string]any, key, parentPath string) (map[string]any, string, error) {
	path := joinPath(parentPath, key)
	raw, ok := obj[key]
	if !ok || raw == nil {
		return nil, path, versionError(path, key, "required field is missing")
	}
	child, err := asObject(raw, path)
	if err != nil {
		return nil, path, err
	}
	return child, path, nil
}

func reqArray(obj map[string]any, key, parentPath string) ([]any, string, error) {
	path := joinPath(parentPath, key)
	raw, ok := obj[key]
	if !ok || raw == nil {
		return nil, path, versionError(path, key, "required field is missing")
	}
	arr, ok := raw.([]any)
	if !ok {
		return nil, path, schemaError(path, key, "expected array, got %s", jsonType(raw))
	}
	return arr, path, nil
}

func reqString(obj map[string]any, key, parentPath string) (string, error) {
	path := joinPath(parentPath, key)
	raw, ok := obj[key]
	if !ok || raw == nil {
		return "", versionError(path, key, "required field is missing")
	}
	return asString(raw, path)
}

func reqBool(obj map[string]any, key, parentPath string) (bool, error) {
	path := joinPath(parentPath, key)
	raw, ok := obj[key]
	if !ok || raw == nil {
		return false, versionError(path, key, "required field is missing")
	}
	return asBool(raw, path)
}

func reqFloat(obj map[string]any, key, parentPath string) (float64, error) {
	path := joinPath(parentPath, key)
	raw, ok := obj[key]
	if !ok || raw == nil {
		return 0, versionError(path, key, "required field is missing")
	}
	return asNumber(raw, path)
}

func reqInt(obj map[string]any, key, parentPath string) (int, error) {
	path := joinPath(parentPath, key)
	raw, ok := obj[key]
	if !ok || raw == nil {
		return 0, versionError(path, key, "required field is missing")
	}
	return asInt(raw, path)
}

func reqInt64(obj map[string]any, key, parentPath string) (int64, error) {
	path := joinPath(parentPath, key)
	raw, ok := obj[key]
	if !ok || raw == nil {
		return 0, versionError(path, key, "required field is missing")
	}
	n, ok := raw.(json.Number)
	if !ok {
		return 0, schemaError(path, key, "expected integer, got %s", jsonType(raw))
	}
	i, err := strconv.ParseInt(n.String(), 10, 64)
	if err != nil {
		return 0, schemaError(path, key, "expected integer, got %q", n)
	}
	return i, nil
}

func reqColor(obj map[string]any, key, parentPath string) ([3]float64, error) {
	path := joinPath(parentPath, key)
	raw, ok := obj[key]
	if !ok || raw == nil {
		return [3]float64{}, versionError(path, key, "required field is missing")
	}
	arr, ok := raw.([]any)
	if !ok {
		return [3]float64{}, schemaError(path, key, "expected array, got %s", jsonType(raw))
	}
	return floatTriplet(arr, path)
}

func floatTriplet(arr []any, path string) ([3]float64, error) {
	if len(arr) != 3 {
		return [3]float64{}, schemaError(path, lastField(path), "expected 3 numbers, got %d", len(arr))
	}
	var out [3]float64
	for i, raw := range arr {
		v, err := asNumber(raw, fmt.Sprintf("%s[%d]", path, i))
		if err != nil {
			return out, err
		}
		out[i] = v
	}
	return out, nil
}

func floatQuad(arr []any, path string) ([4]float64, error) {
	if len(arr) != 4 {
		return [4]float64{}, schemaError(path, lastField(path), "expected 4 numbers, got %d", len(arr))
	}
	var out [4]float64
	for i, raw := range arr {
		v, err := asNumber(raw, fmt.Sprintf("%s[%d]", path, i))
		if err != nil {
			return out, err
		}
		out[i] = v
	}
	return out, nil
}

func jsonType(v any) string {
	switch v.(type) {
	case nil:
		return "null"
	case string:
		return "string"
	case bool:
		return "boolean"
	case json.Number:
		return "number"
	case []any:
		return "array"
	case map[string]any:
		return "object"
	default:
		return fmt.Sprintf("%T", v)
	}
}

func joinPath(parent, key string) string {
	if parent == "" || parent == "$" {
		return "$." + key
	}
	return parent + "." + key
}

func lastField(path string) string {
	idx := strings.LastIndexAny(path, ".]")
	if idx < 0 {
		return strings.TrimPrefix(path, "$.")
	}
	return strings.Trim(path[idx+1:], "[]")
}
