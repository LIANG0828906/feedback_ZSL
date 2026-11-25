package sample

import (
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"math"
	"os"
	"path/filepath"
)

const (
	ProjectID  = "virtual-stage-layer-stack"
	TickRate   = int64(90000)
	WindowSize = 4
	Width      = 64
	Height     = 48
	Channels   = 4
)

type Generated struct {
	Seed  int64
	Files map[string][]byte
}

type layerSpec struct {
	ID           string
	Name         string
	Parent       string
	ContentHash  string
	Opacity      uint16
	Blend        string
	Visible      bool
	VisibleUntil int64
	ParamVersion uint64
}

type instructionSpec struct {
	Sequence     int
	At           int64
	Action       string
	LayerID      string
	Opacity      *uint16
	BlendMode    string
	Visible      *bool
	VisibleUntil *int64
}

type frameSpec struct {
	Index  int
	Time   int64
	Points [][2]int
}

var samplePoints = [][2]int{
	{0, 0},
	{8, 7},
	{21, 13},
	{32, 24},
	{47, 35},
	{63, 47},
}

func Generate(seed int64) (*Generated, error) {
	layers := []layerSpec{
		{
			ID: "plate-backdrop", Name: "Stage Backdrop", Parent: "grp-plate",
			ContentHash: digestFor(seed, "plate-backdrop"), Opacity: 65535,
			Blend: "normal", Visible: true, VisibleUntil: ticksFromMS(1100), ParamVersion: 1,
		},
		{
			ID: "talent-subject", Name: "Talent Subject", Parent: "grp-talent",
			ContentHash: digestFor(seed, "talent-subject"), Opacity: 60000,
			Blend: "normal", Visible: true, VisibleUntil: ticksFromMS(900), ParamVersion: 1,
		},
		{
			ID: "beauty-glow", Name: "Beauty Glow", Parent: "grp-beauty",
			ContentHash: digestFor(seed, "beauty-glow"), Opacity: 50000,
			Blend: "screen", Visible: true, VisibleUntil: ticksFromMS(600), ParamVersion: 1,
		},
		{
			ID: "grade-look", Name: "Grade Look", Parent: "grp-grade",
			ContentHash: digestFor(seed, "grade-look"), Opacity: 40000,
			Blend: "multiply", Visible: true, VisibleUntil: ticksFromMS(1200), ParamVersion: 1,
		},
	}
	opacity := uint16(32768)
	talentOpacity := uint16(50000)
	instructions := []instructionSpec{
		{Sequence: 0, At: ticksFromMS(300), Action: "setOpacity", LayerID: "beauty-glow", Opacity: &opacity},
		{Sequence: 1, At: ticksFromMS(700), Action: "setBlendMode", LayerID: "grade-look", BlendMode: "add"},
		{Sequence: 2, At: ticksFromMS(900), Action: "setOpacity", LayerID: "talent-subject", Opacity: &talentOpacity},
	}
	frames := make([]frameSpec, 12)
	for i := range frames {
		frames[i] = frameSpec{
			Index: i, Time: ticksFromMS(int64(i * 100)),
			Points: append([][2]int(nil), samplePoints...),
		}
	}
	truth := buildTruth(layers, instructions, frames)
	truthObject := truthJSON(truth)
	files := make(map[string][]byte)
	v1, err := buildV1(layers, instructions, frames, truthObject)
	if err != nil {
		return nil, err
	}
	v2, err := buildV2(layers, instructions, frames, truthObject)
	if err != nil {
		return nil, err
	}
	v3, err := buildV3(layers, instructions, frames, truthObject)
	if err != nil {
		return nil, err
	}
	files["project-v1.json"] = v1
	files["project-v2.json"] = v2
	files["project-v3.json"] = v3
	truthFile := map[string]any{
		"seed": seed, "projectId": ProjectID, "tickRate": TickRate,
		"windowSize": WindowSize, "truth": truthObject,
	}
	truthBytes, err := json.MarshalIndent(truthFile, "", "  ")
	if err != nil {
		return nil, err
	}
	files["truth.json"] = truthBytes
	return &Generated{Seed: seed, Files: files}, nil
}

func Write(seed int64, directory string) (*Generated, error) {
	generated, err := Generate(seed)
	if err != nil {
		return nil, err
	}
	if err := os.MkdirAll(directory, 0o755); err != nil {
		return nil, err
	}
	for name, data := range generated.Files {
		if err := os.WriteFile(filepath.Join(directory, name), data, 0o644); err != nil {
			return nil, err
		}
	}
	return generated, nil
}

func digestFor(seed int64, layerID string) string {
	sum := sha256.Sum256([]byte(fmt.Sprintf("vpcompose-seed:%d:layer:%s", seed, layerID)))
	return hex.EncodeToString(sum[:])
}

func ticksFromMS(ms int64) int64 {
	return ms * TickRate / 1000
}

func msFromTicks(ticks int64) int64 {
	return (ticks*1000 + TickRate/2) / TickRate
}

func buildV1(layers []layerSpec, instructions []instructionSpec, frames []frameSpec, truth map[string]any) ([]byte, error) {
	layerObjects := make([]map[string]any, 0, len(layers))
	for _, layer := range layers {
		layerObjects = append(layerObjects, map[string]any{
			"layer_id": layer.ID, "name": layer.Name, "content_sha256": layer.ContentHash,
			"opacity": int(layer.Opacity), "blend_mode": layer.Blend,
			"enabled": layer.Visible, "visible_until_ms": msFromTicks(layer.VisibleUntil),
		})
	}
	instructionObjects := make([]map[string]any, 0, len(instructions))
	for _, instruction := range instructions {
		m := map[string]any{
			"sequence": instruction.Sequence, "at_ms": msFromTicks(instruction.At),
			"action": instruction.Action, "layer_id": instruction.LayerID,
		}
		switch instruction.Action {
		case "setOpacity":
			m["action"] = "set_opacity"
			m["opacity"] = int(*instruction.Opacity)
		case "setBlendMode":
			m["action"] = "set_blend_mode"
			m["blend_mode"] = instruction.BlendMode
		}
		instructionObjects = append(instructionObjects, m)
	}
	frameObjects := frameJSON(frames, "time_ms")
	out := map[string]any{
		"format": "vp-foreground", "schema_version": 1, "project_id": ProjectID,
		"frame":       map[string]any{"width": Width, "height": Height, "channels": Channels},
		"timebase":    map[string]any{"unit": "ms", "tick_rate": TickRate, "rounding": "half-up"},
		"window_size": WindowSize, "layers": layerObjects,
		"instructions": instructionObjects, "frames": frameObjects, "truth": truth,
	}
	return json.MarshalIndent(out, "", "  ")
}

func buildV2(layers []layerSpec, instructions []instructionSpec, frames []frameSpec, truth map[string]any) ([]byte, error) {
	nodeObjects := []map[string]any{
		{"nodeId": "grp-plate", "kind": "group", "name": "Plate"},
		layerNodeV2(layers[0]),
		{"nodeId": "grp-talent", "kind": "group", "name": "Talent"},
		layerNodeV2(layers[1]),
		{"nodeId": "grp-beauty", "kind": "group", "name": "Beauty", "parentId": "grp-talent"},
		layerNodeV2(layers[2]),
		{"nodeId": "grp-grade", "kind": "group", "name": "Grade"},
		layerNodeV2(layers[3]),
	}
	instructionObjects := make([]map[string]any, 0, len(instructions))
	for _, instruction := range instructions {
		m := map[string]any{
			"seq": instruction.Sequence, "atTick": instruction.At,
			"op": instruction.Action, "layerId": instruction.LayerID,
		}
		switch instruction.Action {
		case "setOpacity":
			m["opacity"] = int(*instruction.Opacity)
		case "setBlendMode":
			m["blendMode"] = instruction.BlendMode
		}
		instructionObjects = append(instructionObjects, m)
	}
	out := map[string]any{
		"format": "vp-foreground", "schemaVersion": 2, "projectId": ProjectID,
		"frameSize":  map[string]any{"width": Width, "height": Height, "channels": Channels},
		"timebase":   map[string]any{"unit": "tick", "tickRate": TickRate, "rounding": "half-up"},
		"windowSize": WindowSize, "nodes": nodeObjects,
		"instructions": instructionObjects, "frames": frameJSON(frames, "timeTick"), "truth": truth,
	}
	return json.MarshalIndent(out, "", "  ")
}

func layerNodeV2(layer layerSpec) map[string]any {
	return map[string]any{
		"nodeId": layer.ID, "kind": "layer", "name": layer.Name, "parentId": layer.Parent,
		"layerId": layer.ID, "contentSha256": layer.ContentHash,
		"opacity": int(layer.Opacity), "blendMode": layer.Blend,
		"visible": layer.Visible, "visibleUntilTick": layer.VisibleUntil,
		"alphaMode": "premultiplied",
	}
}

func buildV3(layers []layerSpec, instructions []instructionSpec, frames []frameSpec, truth map[string]any) ([]byte, error) {
	layerByID := make(map[string]layerSpec, len(layers))
	for _, layer := range layers {
		layerByID[layer.ID] = layer
	}
	tree := []map[string]any{
		{
			"node-id": "grp-plate", "kind": "group", "name": "Plate",
			"children": []map[string]any{layerNodeV3(layerByID["plate-backdrop"])},
		},
		{
			"node-id": "grp-talent", "kind": "group", "name": "Talent",
			"children": []map[string]any{
				layerNodeV3(layerByID["talent-subject"]),
				{
					"node-id": "grp-beauty", "kind": "group", "name": "Beauty",
					"parent-id": "grp-talent",
					"children":  []map[string]any{layerNodeV3(layerByID["beauty-glow"])},
				},
			},
		},
		{
			"node-id": "grp-grade", "kind": "group", "name": "Grade",
			"children": []map[string]any{layerNodeV3(layerByID["grade-look"])},
		},
	}
	instructionObjects := make([]map[string]any, 0, len(instructions))
	for _, instruction := range instructions {
		m := map[string]any{
			"seq": instruction.Sequence, "at-tick": instruction.At,
			"op": instruction.Action, "layer-id": instruction.LayerID,
		}
		switch instruction.Action {
		case "setOpacity":
			m["opacity"] = int(*instruction.Opacity)
		case "setBlendMode":
			m["blend-mode"] = instruction.BlendMode
		}
		instructionObjects = append(instructionObjects, m)
	}
	out := map[string]any{
		"format": "vp-foreground", "schema-version": 3, "project-id": ProjectID,
		"frame-size":  map[string]any{"width": Width, "height": Height, "channels": Channels},
		"timebase":    map[string]any{"unit": "tick", "tick-rate": TickRate, "rounding": "half-up"},
		"window-size": WindowSize, "tree": tree,
		"instructions": instructionObjects, "frames": frameJSON(frames, "time-tick"), "truth": truth,
	}
	return json.MarshalIndent(out, "", "  ")
}

func layerNodeV3(layer layerSpec) map[string]any {
	return map[string]any{
		"node-id": layer.ID, "kind": "layer", "name": layer.Name, "parent-id": layer.Parent,
		"layer-id": layer.ID, "content": map[string]any{"sha256": layer.ContentHash},
		"opacity": int(layer.Opacity), "blend-mode": layer.Blend,
		"visibility": map[string]any{"visible": layer.Visible, "until-tick": layer.VisibleUntil},
		"alpha":      map[string]any{"mode": "premultiplied"},
	}
}

func frameJSON(frames []frameSpec, timeKey string) []map[string]any {
	out := make([]map[string]any, 0, len(frames))
	for _, frame := range frames {
		points := make([]map[string]any, 0, len(frame.Points))
		for _, point := range frame.Points {
			points = append(points, map[string]any{"x": point[0], "y": point[1]})
		}
		timeValue := frame.Time
		if timeKey == "time_ms" {
			timeValue = msFromTicks(frame.Time)
		}
		out = append(out, map[string]any{
			"index": frame.Index, timeKey: timeValue, "sample_points": points,
		})
	}
	return out
}

func truthJSON(truth []map[string]any) map[string]any {
	return map[string]any{"tolerance": 0, "frames": truth}
}

// The analytic truth path below deliberately does not call vpcompose. It
// implements the declared fixed-point equations a second time so the domain
// compositor and the fixture generator cross-check each other.

type analyticLayer struct {
	ID           string
	ContentHash  string
	Opacity      uint16
	Blend        string
	Visible      bool
	VisibleUntil int64
}

type analyticInstruction struct {
	At        int64
	LayerID   string
	Opacity   *uint16
	BlendMode string
}

func buildTruth(layers []layerSpec, instructions []instructionSpec, frames []frameSpec) []map[string]any {
	analyticLayers := make([]analyticLayer, 0, len(layers))
	ends := make(map[string]int64, len(layers))
	for _, layer := range layers {
		analyticLayers = append(analyticLayers, analyticLayer{
			ID: layer.ID, ContentHash: layer.ContentHash, Opacity: layer.Opacity,
			Blend: layer.Blend, Visible: layer.Visible, VisibleUntil: layer.VisibleUntil,
		})
		ends[layer.ID] = layer.VisibleUntil
	}
	analyticInstructions := make([]analyticInstruction, 0, len(instructions))
	for _, instruction := range instructions {
		analyticInstructions = append(analyticInstructions, analyticInstruction{
			At: instruction.At, LayerID: instruction.LayerID,
			Opacity: instruction.Opacity, BlendMode: instruction.BlendMode,
		})
	}
	out := make([]map[string]any, 0, len(frames))
	for _, frame := range frames {
		states := append([]analyticLayer(nil), analyticLayers...)
		for _, instruction := range analyticInstructions {
			if instruction.At > frame.Time {
				continue
			}
			for i := range states {
				if states[i].ID != instruction.LayerID {
					continue
				}
				if instruction.Opacity != nil {
					states[i].Opacity = *instruction.Opacity
				}
				if instruction.BlendMode != "" {
					states[i].Blend = instruction.BlendMode
				}
			}
		}
		pixels := make([]map[string]any, 0, len(frame.Points))
		for _, point := range frame.Points {
			rgba := analyticCompositePixel(states, point[0], point[1], frame.Time)
			pixels = append(pixels, map[string]any{
				"x": point[0], "y": point[1],
				"rgba": []int{int(rgba[0]), int(rgba[1]), int(rgba[2]), int(rgba[3])},
			})
		}
		layerEnds := make(map[string]int64, len(ends))
		for id, value := range ends {
			layerEnds[id] = value
		}
		out = append(out, map[string]any{
			"index": frame.Index, "timeTick": frame.Time,
			"layerEnds": layerEnds, "samplePixels": pixels,
		})
	}
	return out
}

func analyticCompositePixel(layers []analyticLayer, x, y int, timeTicks int64) [4]uint16 {
	var dst [4]uint16
	for _, layer := range layers {
		if !layer.Visible || timeTicks > layer.VisibleUntil {
			continue
		}
		content := analyticContentPixel(layer.ContentHash, x, y)
		alpha := analyticMul16(uint32(content[3]), uint32(layer.Opacity))
		src := [4]uint16{
			analyticSRGB(uint8(content[0] >> 8)),
			analyticSRGB(uint8(content[1] >> 8)),
			analyticSRGB(uint8(content[2] >> 8)),
			uint16(alpha),
		}
		for i := 0; i < 3; i++ {
			src[i] = analyticPremultiply(src[i], uint16(alpha))
		}
		dst = analyticComposite(dst, src, layer.Blend)
	}
	return dst
}

func analyticContentPixel(contentHash string, x, y int) [4]uint16 {
	var coordinate [16]byte
	binary.LittleEndian.PutUint64(coordinate[0:8], uint64(int64(x)))
	binary.LittleEndian.PutUint64(coordinate[8:16], uint64(int64(y)))
	h := sha256.New()
	h.Write([]byte(contentHash))
	h.Write([]byte{0})
	h.Write(coordinate[:])
	sum := h.Sum(nil)
	alpha8 := uint16(128 + int(sum[0])%128)
	return [4]uint16{
		uint16(sum[1]) * 257, uint16(sum[2]) * 257, uint16(sum[3]) * 257, alpha8 * 257,
	}
}

func analyticSRGB(channel uint8) uint16 {
	value := float64(channel) / 255.0
	var linear float64
	if value <= 0.04045 {
		linear = value / 12.92
	} else {
		linear = math.Pow((value+0.055)/1.055, 2.4)
	}
	return uint16(math.Round(linear * 65535.0))
}

func analyticMul16(a, b uint32) uint32 {
	if a == 0 || b == 0 {
		return 0
	}
	return (a*b + 32767) / 65535
}

func analyticPremultiply(channel, alpha uint16) uint16 {
	if channel == 0 || alpha == 0 {
		return 0
	}
	if alpha == 65535 {
		return channel
	}
	return uint16((uint32(channel)*uint32(alpha) + 32767) / 65535)
}

func analyticUnpremultiply(channel, alpha uint16) uint16 {
	if channel == 0 || alpha == 0 {
		return 0
	}
	if alpha == 65535 {
		return channel
	}
	value := (uint32(channel)*65535 + uint32(alpha)/2) / uint32(alpha)
	if value > 65535 {
		return 65535
	}
	return uint16(value)
}

func analyticComposite(dst, src [4]uint16, mode string) [4]uint16 {
	as := uint32(src[3])
	if as == 0 {
		return dst
	}
	da := uint32(dst[3])
	if as == 65535 && da == 0 {
		return src
	}
	var cb [3]uint16
	if da > 0 {
		cb[0] = analyticUnpremultiply(dst[0], uint16(da))
		cb[1] = analyticUnpremultiply(dst[1], uint16(da))
		cb[2] = analyticUnpremultiply(dst[2], uint16(da))
	}
	var out [4]uint16
	oneMinusAs := uint32(65535) - as
	for i := 0; i < 3; i++ {
		termDst := analyticMul16(oneMinusAs, uint32(dst[i]))
		termSrc := analyticMul16(uint32(65535)-da, uint32(src[i]))
		var termBlend uint32
		if da > 0 {
			cs := analyticUnpremultiply(src[i], uint16(as))
			bs := analyticBlend(mode, cb[i], cs)
			termBlend = analyticMul16(as, analyticMul16(da, uint32(bs)))
		}
		value := termDst + termSrc + termBlend
		if value > 65535 {
			value = 65535
		}
		out[i] = uint16(value)
	}
	alpha := as + analyticMul16(da, oneMinusAs)
	if alpha > 65535 {
		alpha = 65535
	}
	out[3] = uint16(alpha)
	return out
}

func analyticBlend(mode string, backdrop, source uint16) uint16 {
	cb, cs := uint32(backdrop), uint32(source)
	var value uint32
	switch mode {
	case "multiply":
		value = analyticMul16(cb, cs)
	case "screen":
		value = cb + cs - analyticMul16(cb, cs)
	case "add":
		value = cb + cs
	default:
		value = cs
	}
	if value > 65535 {
		return 65535
	}
	return uint16(value)
}
