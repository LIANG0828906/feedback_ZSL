package model

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"math"
	"sort"
)

const (
	AlgorithmVersion = "vcmp/1.0.0"
	SceneSchema      = "internal/v1"
	ChannelsRGBA     = 4
	MaxDimension     = 16384
	MaxFrameBytes    = 256 << 20
	MaxFrameCount    = 1_000_000
)

type AlphaMode string

const (
	AlphaStraight   AlphaMode = "straight"
	AlphaPremul     AlphaMode = "premultiplied"
	AlphaAssociated AlphaMode = "associated"
)

func (a AlphaMode) Valid() bool {
	return a == AlphaStraight || a == AlphaPremul || a == AlphaAssociated
}

// Layer uses canonical straight-alpha sRGB bytes internally. SourceAlphaMode
// records how bytes were encoded in the source document before normalization.
type Layer struct {
	ID              string    `json:"id"`
	Name            string    `json:"name"`
	Visible         bool      `json:"visible"`
	Opacity         float64   `json:"opacity"`
	SourceAlphaMode AlphaMode `json:"source_alpha_mode"`
	Pixels          []byte    `json:"pixels"`
}

type Scene struct {
	SourceVersion   string  `json:"source_version"`
	SourcePath      string  `json:"source_path,omitempty"`
	Width           int     `json:"width"`
	Height          int     `json:"height"`
	Channels        int     `json:"channels"`
	FrameCount      int     `json:"frame_count"`
	FrameDurationNS int64   `json:"frame_duration_ns"`
	Layers          []Layer `json:"layers"`
	OrderExpr       string  `json:"order_expression"`
}

func (s *Scene) Clone() *Scene {
	if s == nil {
		return nil
	}
	cp := *s
	cp.Layers = make([]Layer, len(s.Layers))
	for i := range s.Layers {
		cp.Layers[i] = s.Layers[i]
		cp.Layers[i].Pixels = append([]byte(nil), s.Layers[i].Pixels...)
	}
	return &cp
}

func (s *Scene) LayerByID(id string) (*Layer, int, bool) {
	for i := range s.Layers {
		if s.Layers[i].ID == id {
			return &s.Layers[i], i, true
		}
	}
	return nil, -1, false
}

func (s *Scene) FrameBytes() int64 {
	return int64(s.Width) * int64(s.Height) * int64(s.Channels)
}

func (s *Scene) TimeForFrame(frame int) int64 {
	return int64(frame) * s.FrameDurationNS
}

// BaseHash identifies the immutable scene structure. Editable layer
// parameters, visibility and frame duration are intentionally excluded; they
// are represented by each window's effective parameter version.
func (s *Scene) BaseHash() string {
	type layerBase struct {
		ID     string `json:"id"`
		Name   string `json:"name"`
		Pixels string `json:"pixels_sha256"`
	}
	doc := struct {
		Version    string      `json:"version"`
		Dimensions [3]int      `json:"dimensions"`
		FrameCount int         `json:"frame_count"`
		OrderExpr  string      `json:"order_expression"`
		Layers     []layerBase `json:"layers"`
	}{
		Version:    s.SourceVersion,
		Dimensions: [3]int{s.Width, s.Height, s.Channels},
		FrameCount: s.FrameCount,
		OrderExpr:  s.OrderExpr,
	}
	for _, layer := range s.Layers {
		doc.Layers = append(doc.Layers, layerBase{
			ID:     layer.ID,
			Name:   layer.Name,
			Pixels: HashBytes(layer.Pixels),
		})
	}
	b, _ := json.Marshal(doc)
	return HashBytes(b)
}

type CommandSet struct {
	Version      string         `json:"version"`
	WindowFrames int            `json:"window_frames"`
	Frames       []FrameCommand `json:"frames"`
}

type FrameCommand struct {
	Frame   int                `json:"frame"`
	Visible map[string]bool    `json:"visible"`
	Opacity map[string]float64 `json:"opacity,omitempty"`
}

func (c *CommandSet) Clone() *CommandSet {
	if c == nil {
		return nil
	}
	cp := *c
	cp.Frames = make([]FrameCommand, len(c.Frames))
	for i, f := range c.Frames {
		cp.Frames[i].Frame = f.Frame
		cp.Frames[i].Visible = cloneBoolMap(f.Visible)
		cp.Frames[i].Opacity = cloneFloatMap(f.Opacity)
	}
	return &cp
}

func cloneBoolMap(in map[string]bool) map[string]bool {
	if in == nil {
		return nil
	}
	out := make(map[string]bool, len(in))
	for k, v := range in {
		out[k] = v
	}
	return out
}

func cloneFloatMap(in map[string]float64) map[string]float64 {
	if in == nil {
		return nil
	}
	out := make(map[string]float64, len(in))
	for k, v := range in {
		out[k] = v
	}
	return out
}

type EffectiveLayer struct {
	ID      string  `json:"id"`
	Visible bool    `json:"visible"`
	Opacity float64 `json:"opacity"`
}

func EffectiveLayerState(scene *Scene, cmd *FrameCommand, layer Layer) EffectiveLayer {
	visible := layer.Visible
	opacity := layer.Opacity
	if cmd != nil {
		if v, ok := cmd.Visible[layer.ID]; ok {
			visible = visible && v
		}
		if v, ok := cmd.Opacity[layer.ID]; ok {
			opacity *= v
		}
	}
	if opacity < 0 {
		opacity = 0
	}
	return EffectiveLayer{ID: layer.ID, Visible: visible, Opacity: opacity}
}

func (s *Scene) EffectiveWindow(states []EffectiveLayer, frame int) []EffectiveLayer {
	if len(states) == 0 {
		return nil
	}
	out := make([]EffectiveLayer, len(states))
	copy(out, states)
	return out
}

type LayerSnapshot struct {
	ID              string           `json:"id"`
	Name            string           `json:"name"`
	Order           int              `json:"order"`
	BaseVisible     bool             `json:"base_visible"`
	BaseOpacity     float64          `json:"base_opacity"`
	SourceAlphaMode AlphaMode        `json:"source_alpha_mode"`
	PixelsHash      string           `json:"pixels_hash"`
	FrameStates     []EffectiveLayer `json:"frame_states"`
	FrameOpacity    map[int]float64  `json:"frame_opacity,omitempty"`
}

type WindowSnapshot struct {
	WindowIndex     int             `json:"window_index"`
	StartFrame      int             `json:"start_frame"`
	EndFrame        int             `json:"end_frame"`
	Width           int             `json:"width"`
	Height          int             `json:"height"`
	Channels        int             `json:"channels"`
	FrameDurationNS int64           `json:"frame_duration_ns"`
	SourceVersion   string          `json:"source_version"`
	ParamVersion    string          `json:"param_version"`
	ContentHash     string          `json:"content_hash"`
	Layers          []LayerSnapshot `json:"layers"`
}

func (w *WindowSnapshot) Clone() *WindowSnapshot {
	if w == nil {
		return nil
	}
	b, _ := json.Marshal(w)
	var out WindowSnapshot
	_ = json.Unmarshal(b, &out)
	return &out
}

type PixelSample struct {
	X    int      `json:"x"`
	Y    int      `json:"y"`
	RGBA [4]uint8 `json:"rgba"`
}

type FrameStats struct {
	Frame         int           `json:"frame"`
	TimeNS        int64         `json:"time_ns"`
	ContentHash   string        `json:"content_hash"`
	VisibleLayers []string      `json:"visible_layers"`
	Samples       []PixelSample `json:"samples"`
	MeanRGBA      [4]float64    `json:"mean_rgba"`
	MinLuma       uint8         `json:"min_luma"`
	MaxLuma       uint8         `json:"max_luma"`
}

type Frame struct {
	Index    int        `json:"index"`
	Width    int        `json:"width"`
	Height   int        `json:"height"`
	Channels int        `json:"channels"`
	TimeNS   int64      `json:"time_ns"`
	RGBA     []byte     `json:"rgba"`
	Hash     string     `json:"hash"`
	Stats    FrameStats `json:"stats"`
}

func (f Frame) Clone() Frame {
	cp := f
	cp.RGBA = append([]byte(nil), f.RGBA...)
	cp.Stats.Samples = append([]PixelSample(nil), f.Stats.Samples...)
	cp.Stats.VisibleLayers = append([]string(nil), f.Stats.VisibleLayers...)
	return cp
}

func (f *Frame) Pixel(x, y int) [4]uint8 {
	var out [4]uint8
	if f == nil || x < 0 || y < 0 || x >= f.Width || y >= f.Height {
		return out
	}
	off := (y*f.Width + x) * f.Channels
	if off+4 > len(f.RGBA) {
		return out
	}
	copy(out[:], f.RGBA[off:off+4])
	return out
}

type SamplePoint struct {
	X int `json:"x"`
	Y int `json:"y"`
}

type VisibilityEndpoint struct {
	FirstVisible int  `json:"first_visible_frame"`
	LastVisible  int  `json:"last_visible_frame"`
	EverVisible  bool `json:"ever_visible"`
}

type TruthFrame struct {
	Frame       int             `json:"frame"`
	TimeNS      int64           `json:"time_ns"`
	Visible     map[string]bool `json:"visible"`
	Samples     []PixelSample   `json:"samples"`
	ContentHash string          `json:"content_hash"`
}

type Truth struct {
	Format          string                        `json:"format"`
	Version         string                        `json:"version"`
	FrameCount      int                           `json:"frame_count"`
	WindowFrames    int                           `json:"window_frames"`
	SamplePoints    []SamplePoint                 `json:"sample_points"`
	LayerVisibility map[string]VisibilityEndpoint `json:"layer_visibility"`
	Frames          []TruthFrame                  `json:"frames"`
}

func HashBytes(b []byte) string {
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:])
}

func HashString(s string) string {
	return HashBytes([]byte(s))
}

func SortedKeys[V any](m map[string]V) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}

func ParamVersionFor(snapshot *WindowSnapshot) string {
	type stable struct {
		SourceVersion string `json:"source_version"`
		StartFrame    int    `json:"start_frame"`
		EndFrame      int    `json:"end_frame"`
		RenderHash    string `json:"render_hash"`
	}
	doc := stable{
		SourceVersion: snapshot.SourceVersion,
		StartFrame:    snapshot.StartFrame,
		EndFrame:      snapshot.EndFrame,
		RenderHash:    SnapshotContentHash(snapshot),
	}
	b, _ := json.Marshal(doc)
	return HashBytes(b)
}

func WindowContentHash(scene *Scene, commands *CommandSet, startFrame, endFrame int) string {
	type frameState struct {
		Frame  int              `json:"frame"`
		TimeNS int64            `json:"time_ns"`
		Layers []EffectiveLayer `json:"layers"`
	}
	doc := struct {
		Algorithm   string       `json:"algorithm"`
		Dimensions  [3]int       `json:"dimensions"`
		StartFrame  int          `json:"start_frame"`
		EndFrame    int          `json:"end_frame"`
		FrameData   []frameState `json:"frames"`
		LayerHashes []string     `json:"layer_hashes"`
	}{Algorithm: AlgorithmVersion, Dimensions: [3]int{scene.Width, scene.Height, scene.Channels}, StartFrame: startFrame, EndFrame: endFrame}
	for _, layer := range scene.Layers {
		doc.LayerHashes = append(doc.LayerHashes, layer.ID+":"+HashBytes(layer.Pixels))
	}
	for frame := startFrame; frame <= endFrame; frame++ {
		cmd := commandFor(commands, frame)
		fs := frameState{Frame: frame, TimeNS: scene.TimeForFrame(frame)}
		for _, layer := range scene.Layers {
			fs.Layers = append(fs.Layers, renderContribution(EffectiveLayerState(scene, cmd, layer)))
		}
		doc.FrameData = append(doc.FrameData, fs)
	}
	b, _ := json.Marshal(doc)
	return HashBytes(b)
}

func SnapshotContentHash(snapshot *WindowSnapshot) string {
	type frameState struct {
		Frame  int              `json:"frame"`
		TimeNS int64            `json:"time_ns"`
		Layers []EffectiveLayer `json:"layers"`
	}
	doc := struct {
		Algorithm   string       `json:"algorithm"`
		Dimensions  [3]int       `json:"dimensions"`
		StartFrame  int          `json:"start_frame"`
		EndFrame    int          `json:"end_frame"`
		FrameData   []frameState `json:"frames"`
		LayerHashes []string     `json:"layer_hashes"`
	}{
		Algorithm:  AlgorithmVersion,
		Dimensions: [3]int{snapshot.Width, snapshot.Height, snapshot.Channels},
		StartFrame: snapshot.StartFrame,
		EndFrame:   snapshot.EndFrame,
	}
	for _, layer := range snapshot.Layers {
		doc.LayerHashes = append(doc.LayerHashes, layer.ID+":"+layer.PixelsHash)
		for i, state := range layer.FrameStates {
			frame := snapshot.StartFrame + i
			if i >= len(doc.FrameData) {
				doc.FrameData = append(doc.FrameData, frameState{Frame: frame, TimeNS: snapshot.FrameDurationNS * int64(frame)})
			}
			doc.FrameData[i].Layers = append(doc.FrameData[i].Layers, renderContribution(state))
		}
	}
	b, _ := json.Marshal(doc)
	return HashBytes(b)
}

func renderContribution(state EffectiveLayer) EffectiveLayer {
	if !state.Visible || state.Opacity <= 0 {
		state.Opacity = 0
		state.Visible = false
	}
	return state
}

func commandFor(commands *CommandSet, frame int) *FrameCommand {
	if commands == nil {
		return nil
	}
	if frame >= 0 && frame < len(commands.Frames) && commands.Frames[frame].Frame == frame {
		return &commands.Frames[frame]
	}
	for i := range commands.Frames {
		if commands.Frames[i].Frame == frame {
			return &commands.Frames[i]
		}
	}
	return nil
}

func BuildWindowSnapshot(scene *Scene, commands *CommandSet, windowIndex, startFrame, endFrame int) *WindowSnapshot {
	snap := &WindowSnapshot{
		WindowIndex:     windowIndex,
		StartFrame:      startFrame,
		EndFrame:        endFrame,
		Width:           scene.Width,
		Height:          scene.Height,
		Channels:        scene.Channels,
		FrameDurationNS: scene.FrameDurationNS,
		SourceVersion:   scene.SourceVersion,
	}
	for order, layer := range scene.Layers {
		ls := LayerSnapshot{
			ID:              layer.ID,
			Name:            layer.Name,
			Order:           order,
			BaseVisible:     layer.Visible,
			BaseOpacity:     layer.Opacity,
			SourceAlphaMode: layer.SourceAlphaMode,
			PixelsHash:      HashBytes(layer.Pixels),
			FrameOpacity:    map[int]float64{},
		}
		for frame := startFrame; frame <= endFrame; frame++ {
			cmd := commandFor(commands, frame)
			eff := EffectiveLayerState(scene, cmd, layer)
			ls.FrameStates = append(ls.FrameStates, eff)
			if cmd != nil {
				if mult, ok := cmd.Opacity[layer.ID]; ok {
					ls.FrameOpacity[frame] = mult
				}
			}
		}
		snap.Layers = append(snap.Layers, ls)
	}
	snap.ContentHash = SnapshotContentHash(snap)
	snap.ParamVersion = ParamVersionFor(snap)
	return snap
}

func ValidateScene(scene *Scene) error {
	if scene == nil {
		return NewError(CodeVersion, "scene", "scene is nil")
	}
	if scene.Width <= 0 {
		return NewError(CodeParam, "width", "must be > 0")
	}
	if scene.Height <= 0 {
		return NewError(CodeParam, "height", "must be > 0")
	}
	if scene.Width > MaxDimension || scene.Height > MaxDimension {
		return NewError(CodeParam, "dimensions", fmt.Sprintf("each dimension must be <= %d", MaxDimension))
	}
	if scene.Channels != ChannelsRGBA {
		return NewError(CodeParam, "channels", fmt.Sprintf("only %d-channel RGBA is supported", ChannelsRGBA))
	}
	if int64(scene.Width)*int64(scene.Height)*int64(scene.Channels) > MaxFrameBytes {
		return NewError(CodeParam, "dimensions", fmt.Sprintf("frame buffer must be <= %d bytes", MaxFrameBytes))
	}
	if scene.FrameCount <= 0 {
		return NewError(CodeParam, "frame_count", "must be > 0")
	}
	if scene.FrameCount > MaxFrameCount {
		return NewError(CodeParam, "frame_count", fmt.Sprintf("must be <= %d", MaxFrameCount))
	}
	if scene.FrameDurationNS <= 0 {
		return NewError(CodeParam, "frame_duration_ns", "must be > 0")
	}
	if scene.FrameDurationNS > math.MaxInt64/int64(scene.FrameCount) {
		return NewError(CodeParam, "frame_duration_ns", "frame timestamps would overflow int64 nanoseconds")
	}
	if len(scene.Layers) == 0 {
		return NewError(CodeVersion, "layers", "at least one layer is required")
	}
	expected := scene.Width * scene.Height * scene.Channels
	seen := make(map[string]struct{}, len(scene.Layers))
	for i := range scene.Layers {
		layer := &scene.Layers[i]
		path := fmt.Sprintf("layers[%d]", i)
		if layer.ID == "" {
			return NewError(CodeVersion, path+".id", "required field is empty")
		}
		if _, ok := seen[layer.ID]; ok {
			return NewError(CodeVersion, path+".id", "duplicate layer id")
		}
		seen[layer.ID] = struct{}{}
		if math.IsNaN(layer.Opacity) || math.IsInf(layer.Opacity, 0) || layer.Opacity < 0 || layer.Opacity > 1 {
			return NewError(CodeParam, path+".opacity", "must be in [0,1]")
		}
		if !layer.SourceAlphaMode.Valid() {
			return NewError(CodeVersion, path+".alpha_mode", "unknown alpha convention")
		}
		if len(layer.Pixels) != expected {
			return NewError(CodeDecode, path+".pixels", fmt.Sprintf("decoded length %d, expected %d", len(layer.Pixels), expected))
		}
	}
	return nil
}

func ValidateCommands(scene *Scene, commands *CommandSet) error {
	if commands == nil {
		return NewError(CodeVersion, "commands", "commands are nil")
	}
	if commands.WindowFrames <= 0 {
		return NewError(CodeParam, "commands.window_frames", "must be > 0")
	}
	if len(commands.Frames) != scene.FrameCount {
		return NewError(CodeVersion, "commands.frames", fmt.Sprintf("count %d does not match frame_count %d", len(commands.Frames), scene.FrameCount))
	}
	layerIDs := make(map[string]struct{}, len(scene.Layers))
	for _, layer := range scene.Layers {
		layerIDs[layer.ID] = struct{}{}
	}
	for i := range commands.Frames {
		cmd := &commands.Frames[i]
		path := fmt.Sprintf("commands.frames[%d]", i)
		if cmd.Frame != i {
			return NewError(CodeVersion, path+".frame", fmt.Sprintf("expected frame %d", i))
		}
		for id := range cmd.Visible {
			if _, ok := layerIDs[id]; !ok {
				return NewError(CodeVersion, path+".visible."+id, "unknown layer reference")
			}
		}
		for id, opacity := range cmd.Opacity {
			if _, ok := layerIDs[id]; !ok {
				return NewError(CodeVersion, path+".opacity."+id, "unknown layer reference")
			}
			if math.IsNaN(opacity) || math.IsInf(opacity, 0) || opacity < 0 || opacity > 1 {
				return NewError(CodeParam, path+".opacity."+id, "must be in [0,1]")
			}
		}
	}
	return nil
}
