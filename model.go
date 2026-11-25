package vpcompose

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"sort"
	"strings"
)

const AlgorithmVersion = "vpcompose-linear16-v1"

type Version int

const (
	Version1 Version = 1
	Version2 Version = 2
	Version3 Version = 3
)

func (v Version) String() string { return fmt.Sprintf("v%d", int(v)) }

func (v Version) Valid() bool { return v >= Version1 && v <= Version3 }

type AlphaMode string

const (
	AlphaStraight      AlphaMode = "straight"
	AlphaPremultiplied AlphaMode = "premultiplied"
)

type BlendMode string

const (
	BlendNormal   BlendMode = "normal"
	BlendAdd      BlendMode = "add"
	BlendMultiply BlendMode = "multiply"
	BlendScreen   BlendMode = "screen"
)

func (b BlendMode) Valid() bool {
	switch b {
	case BlendNormal, BlendAdd, BlendMultiply, BlendScreen:
		return true
	default:
		return false
	}
}

type Rounding string

const (
	RoundDown    Rounding = "down"
	RoundUp      Rounding = "up"
	RoundHalfUp  Rounding = "half-up"
	RoundFloor   Rounding = "floor"
	RoundCeil    Rounding = "ceil"
	RoundNearest Rounding = "nearest"
)

func (r Rounding) Valid() bool {
	switch r {
	case RoundDown, RoundUp, RoundHalfUp, RoundFloor, RoundCeil, RoundNearest:
		return true
	default:
		return false
	}
}

type Timebase struct {
	Unit     string   `json:"unit"`
	TickRate int64    `json:"tickRate"`
	Rounding Rounding `json:"rounding"`
}

func InternalTimebase(tickRate int64) Timebase {
	return Timebase{Unit: "tick", TickRate: tickRate, Rounding: RoundHalfUp}
}

type NodeKind string

const (
	NodeGroup NodeKind = "group"
	NodeLayer NodeKind = "layer"
)

type Node struct {
	ID       string   `json:"id"`
	Kind     NodeKind `json:"kind"`
	Name     string   `json:"name,omitempty"`
	ParentID string   `json:"parentId,omitempty"`
	Children []string `json:"children,omitempty"`
	LayerID  string   `json:"layerId,omitempty"`
}

type Layer struct {
	ID              string    `json:"id"`
	Name            string    `json:"name"`
	ParentID        string    `json:"parentId,omitempty"`
	ContentHash     string    `json:"contentHash"`
	Opacity         uint16    `json:"opacity"`
	BlendMode       BlendMode `json:"blendMode"`
	Visible         bool      `json:"visible"`
	VisibleUntil    int64     `json:"visibleUntilTick"`
	AlphaMode       AlphaMode `json:"alphaMode"`
	SourceAlphaMode AlphaMode `json:"sourceAlphaMode"`
	ParamVersion    uint64    `json:"paramVersion"`
}

type InstructionAction string

const (
	InstructionSetVisible      InstructionAction = "set_visible"
	InstructionSetOpacity      InstructionAction = "set_opacity"
	InstructionSetVisibleUntil InstructionAction = "set_visible_until"
	InstructionSetBlendMode    InstructionAction = "set_blend_mode"
)

// Instruction is expressed in internal ticks. Pointer fields distinguish an
// absent patch member from a valid zero value.
type Instruction struct {
	Sequence     int               `json:"sequence"`
	At           int64             `json:"atTick"`
	Action       InstructionAction `json:"action"`
	LayerID      string            `json:"layerId"`
	Visible      *bool             `json:"visible,omitempty"`
	Opacity      *uint16           `json:"opacity,omitempty"`
	VisibleUntil *int64            `json:"visibleUntilTick,omitempty"`
	BlendMode    BlendMode         `json:"blendMode,omitempty"`
}

type Point struct {
	X int `json:"x"`
	Y int `json:"y"`
}

type FrameSpec struct {
	Index        int     `json:"index"`
	TimeTicks    int64   `json:"timeTick"`
	SamplePoints []Point `json:"samplePoints"`
}

type TruthPixel struct {
	X    int       `json:"x"`
	Y    int       `json:"y"`
	RGBA [4]uint16 `json:"rgba"`
}

type FrameTruth struct {
	Index        int              `json:"index"`
	TimeTicks    int64            `json:"timeTick"`
	LayerEnds    map[string]int64 `json:"layerEnds"`
	SamplePixels []TruthPixel     `json:"samplePixels"`
}

type Truth struct {
	Tolerance int          `json:"tolerance"`
	Frames    []FrameTruth `json:"frames"`
}

type ConversionTrace struct {
	Path     string `json:"path"`
	OldValue string `json:"oldValue"`
	NewValue string `json:"newValue"`
	Rule     string `json:"rule"`
	Detail   string `json:"detail,omitempty"`
}

type Project struct {
	ID               string            `json:"id"`
	Width            int               `json:"width"`
	Height           int               `json:"height"`
	Channels         int               `json:"channels"`
	Timebase         Timebase          `json:"timebase"`
	WindowSize       int               `json:"windowSize"`
	Layers           []*Layer          `json:"layers"`
	Nodes            []Node            `json:"nodes"`
	Instructions     []Instruction     `json:"instructions"`
	Frames           []FrameSpec       `json:"frames"`
	Truth            *Truth            `json:"truth,omitempty"`
	SourceVersion    Version           `json:"sourceVersion"`
	SourceFormat     string            `json:"sourceFormat"`
	ParamVersion     uint64            `json:"paramVersion"`
	AlgorithmVersion string            `json:"algorithmVersion"`
	ConversionTraces []ConversionTrace `json:"conversionTraces,omitempty"`
}

func (p *Project) LayerByID(id string) *Layer {
	for _, layer := range p.Layers {
		if layer.ID == id {
			return layer
		}
	}
	return nil
}

func (p *Project) NodeByID(id string) *Node {
	for i := range p.Nodes {
		if p.Nodes[i].ID == id {
			return &p.Nodes[i]
		}
	}
	return nil
}

func (p *Project) LayerOrder() []string {
	out := make([]string, len(p.Layers))
	for i, layer := range p.Layers {
		out[i] = layer.ID
	}
	return out
}

func (p *Project) Clone() *Project {
	b, _ := json.Marshal(p)
	var out Project
	_ = json.Unmarshal(b, &out)
	return &out
}

func (p *Project) Normalize() error {
	if p.ParamVersion == 0 {
		p.ParamVersion = 1
	}
	if p.AlgorithmVersion == "" {
		p.AlgorithmVersion = AlgorithmVersion
	}
	if p.Timebase.TickRate <= 0 {
		return schemaError("timebase.tickRate", "must be positive")
	}
	if p.Timebase.Unit == "" {
		p.Timebase.Unit = "tick"
	}
	if p.Timebase.Unit != "tick" {
		return schemaError("timebase.unit", "internal model requires tick")
	}
	if p.Timebase.Rounding == "" {
		p.Timebase.Rounding = RoundHalfUp
	}
	if !p.Timebase.Rounding.Valid() {
		return schemaError("timebase.rounding", "unsupported rounding rule")
	}
	if p.Width <= 0 {
		return schemaError("frame.width", "must be positive")
	}
	if p.Height <= 0 {
		return schemaError("frame.height", "must be positive")
	}
	if p.Channels != 4 {
		return schemaError("frame.channels", "only 4-channel RGBA is supported")
	}
	if p.WindowSize <= 0 {
		return schemaError("windowSize", "must be positive")
	}
	if len(p.Layers) == 0 {
		return schemaError("layers", "at least one layer is required")
	}
	for i, layer := range p.Layers {
		path := fmt.Sprintf("layers[%d]", i)
		if layer.ID == "" {
			return schemaError(path+".id", "required")
		}
		if layer.ContentHash == "" {
			return schemaError(path+".contentHash", "required")
		}
		if !layer.BlendMode.Valid() {
			return schemaError(path+".blendMode", "unsupported blend mode")
		}
		if layer.AlphaMode == "" {
			layer.AlphaMode = AlphaPremultiplied
		}
		if layer.SourceAlphaMode == "" {
			layer.SourceAlphaMode = layer.AlphaMode
		}
		if layer.VisibleUntil < 0 {
			return schemaError(path+".visibleUntilTick", "must not be negative")
		}
	}
	if len(p.Frames) == 0 {
		return schemaError("frames", "at least one frame is required")
	}
	for i, frame := range p.Frames {
		if frame.Index != i {
			return schemaError(fmt.Sprintf("frames[%d].index", i), "must be contiguous from zero")
		}
		for j, point := range frame.SamplePoints {
			if point.X < 0 || point.X >= p.Width || point.Y < 0 || point.Y >= p.Height {
				return schemaError(fmt.Sprintf("frames[%d].samplePoints[%d]", i, j), "outside frame")
			}
		}
	}
	return nil
}

func (p *Project) ContentHash() string {
	type hashLayer struct {
		ID           string    `json:"id"`
		ParentID     string    `json:"parentId"`
		ContentHash  string    `json:"contentHash"`
		Opacity      uint16    `json:"opacity"`
		BlendMode    BlendMode `json:"blendMode"`
		Visible      bool      `json:"visible"`
		VisibleUntil int64     `json:"visibleUntilTick"`
		AlphaMode    AlphaMode `json:"alphaMode"`
		ParamVersion uint64    `json:"paramVersion"`
	}
	layers := make([]hashLayer, len(p.Layers))
	for i, layer := range p.Layers {
		layers[i] = hashLayer{
			ID: layer.ID, ParentID: layer.ParentID, ContentHash: layer.ContentHash,
			Opacity: layer.Opacity, BlendMode: layer.BlendMode, Visible: layer.Visible,
			VisibleUntil: layer.VisibleUntil, AlphaMode: layer.AlphaMode,
			ParamVersion: layer.ParamVersion,
		}
	}
	payload := struct {
		ID           string        `json:"id"`
		Width        int           `json:"width"`
		Height       int           `json:"height"`
		Channels     int           `json:"channels"`
		Timebase     Timebase      `json:"timebase"`
		WindowSize   int           `json:"windowSize"`
		Layers       []hashLayer   `json:"layers"`
		Instructions []Instruction `json:"instructions"`
		Frames       []FrameSpec   `json:"frames"`
	}{
		ID: p.ID, Width: p.Width, Height: p.Height, Channels: p.Channels,
		Timebase: p.Timebase, WindowSize: p.WindowSize, Layers: layers,
		Instructions: p.Instructions, Frames: p.Frames,
	}
	b, _ := json.Marshal(payload)
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:])
}

func (p *Project) OverallVersion() string {
	payload := struct {
		ID               string      `json:"id"`
		Width            int         `json:"width"`
		Height           int         `json:"height"`
		Channels         int         `json:"channels"`
		WindowSize       int         `json:"windowSize"`
		Frames           []FrameSpec `json:"frames"`
		SourceVersion    Version     `json:"sourceVersion"`
		AlgorithmVersion string      `json:"algorithmVersion"`
	}{
		ID: p.ID, Width: p.Width, Height: p.Height, Channels: p.Channels,
		WindowSize: p.WindowSize, Frames: p.Frames, SourceVersion: p.SourceVersion,
		AlgorithmVersion: p.AlgorithmVersion,
	}
	b, _ := json.Marshal(payload)
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:])
}

func (p *Project) WindowCount() int {
	if p.WindowSize <= 0 {
		return 0
	}
	return (len(p.Frames) + p.WindowSize - 1) / p.WindowSize
}

func (p *Project) WindowBounds(index int) (int, int, bool) {
	if index < 0 || index >= p.WindowCount() {
		return 0, 0, false
	}
	start := index * p.WindowSize
	end := start + p.WindowSize
	if end > len(p.Frames) {
		end = len(p.Frames)
	}
	return start, end, true
}

func (p *Project) ValidateHierarchy() error {
	layerIDs := make(map[string]bool, len(p.Layers))
	for _, layer := range p.Layers {
		if layerIDs[layer.ID] {
			return schemaError("layers", "duplicate layer id "+layer.ID)
		}
		layerIDs[layer.ID] = true
	}
	nodeIDs := make(map[string]bool, len(p.Nodes))
	parents := make(map[string]string, len(p.Nodes))
	for i, node := range p.Nodes {
		if node.ID == "" {
			return schemaError(fmt.Sprintf("nodes[%d].id", i), "required")
		}
		if nodeIDs[node.ID] {
			return schemaError(fmt.Sprintf("nodes[%d].id", i), "duplicate node id")
		}
		nodeIDs[node.ID] = true
		parents[node.ID] = node.ParentID
		if node.Kind == NodeLayer && !layerIDs[node.LayerID] {
			return schemaError(fmt.Sprintf("nodes[%d].layerId", i), "does not reference a layer")
		}
	}
	for _, node := range p.Nodes {
		if node.ParentID != "" && !nodeIDs[node.ParentID] {
			return schemaError("node."+node.ID+".parentId", "unknown parent "+node.ParentID)
		}
	}
	if cycle := findCycle(parents); len(cycle) > 0 {
		return &EngineError{
			Code:     CodeCycle,
			Path:     "nodes",
			Message:  "layer group hierarchy contains a cycle",
			LayerIDs: cycle,
		}
	}
	return nil
}

func findCycle(parents map[string]string) []string {
	ids := make([]string, 0, len(parents))
	for id := range parents {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	state := make(map[string]int, len(parents))
	stack := make([]string, 0, len(parents))
	pos := make(map[string]int, len(parents))
	var walk func(string) []string
	walk = func(id string) []string {
		switch state[id] {
		case 1:
			start := pos[id]
			out := append([]string(nil), stack[start:]...)
			return out
		case 2:
			return nil
		}
		state[id] = 1
		pos[id] = len(stack)
		stack = append(stack, id)
		if parent := parents[id]; parent != "" {
			if cycle := walk(parent); len(cycle) > 0 {
				return cycle
			}
		}
		stack = stack[:len(stack)-1]
		delete(pos, id)
		state[id] = 2
		return nil
	}
	for _, id := range ids {
		if cycle := walk(id); len(cycle) > 0 {
			return cycle
		}
	}
	return nil
}

func joinPath(parts ...string) string {
	return strings.Join(parts, ".")
}
