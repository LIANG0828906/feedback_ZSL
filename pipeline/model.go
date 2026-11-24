package pipeline

import (
	"fmt"
	"math"
	"sort"
	"strings"
)

const (
	AlgorithmVersion = "vsp-composite/1"

	AlphaPremultiplied = "premultiplied"
	AlphaStraight      = "straight"

	ColorSRGB   = "srgb"
	ColorLinear = "linear-srgb"

	ShapeFull    = "full"
	ShapeRect    = "rect"
	ShapeEllipse = "ellipse"
)

// Rational is kept in reduced form for stable serialization and exact frame
// timestamp comparisons.
type Rational struct {
	Num int64 `json:"num"`
	Den int64 `json:"den"`
}

func NewRational(num, den int64) Rational {
	if den < 0 {
		num, den = -num, -den
	}
	if den == 0 {
		return Rational{Num: num, Den: 1}
	}
	g := gcd(abs64(num), abs64(den))
	if g == 0 {
		g = 1
	}
	return Rational{Num: num / g, Den: den / g}
}

func (r Rational) Add(other Rational) Rational {
	return NewRational(r.Num*other.Den+other.Num*r.Den, r.Den*other.Den)
}

func (r Rational) MulInt(n int64) Rational {
	return NewRational(r.Num*n, r.Den)
}

func (r Rational) Float64() float64 {
	return float64(r.Num) / float64(r.Den)
}

func (r Rational) Equal(other Rational) bool {
	a, b := r.Reduced(), other.Reduced()
	return a.Num == b.Num && a.Den == b.Den
}

func (r Rational) Reduced() Rational {
	return NewRational(r.Num, r.Den)
}

func gcd(a, b int64) int64 {
	for b != 0 {
		a, b = b, a%b
	}
	return a
}

func abs64(v int64) int64 {
	if v < 0 {
		return -v
	}
	return v
}

// TimeBase describes one frame duration in the unit used by the source. The
// canonical internal timeline converts every source declaration to rational
// seconds while retaining the original declarations in SourceTimeBases.
type TimeBase struct {
	Unit          string   `json:"unit"`
	FramePeriod   Rational `json:"frame_period"`
	TicksPerFrame int64    `json:"ticks_per_frame,omitempty"`
	TickRate      int64    `json:"tick_rate,omitempty"`
}

func (t TimeBase) Seconds() Rational {
	switch t.Unit {
	case "second", "seconds":
		return t.FramePeriod.Reduced()
	case "tick", "ticks":
		if t.TickRate <= 0 {
			return Rational{}
		}
		return NewRational(t.TicksPerFrame, t.TickRate)
	default:
		return Rational{}
	}
}

func (t TimeBase) Timestamp(frame int) Rational {
	if (t.Unit == "tick" || t.Unit == "ticks") && t.TicksPerFrame > 0 {
		return NewRational(int64(frame)*t.TicksPerFrame, 1)
	}
	return t.FramePeriod.MulInt(int64(frame))
}

type Canvas struct {
	Width      int    `json:"width"`
	Height     int    `json:"height"`
	Channels   int    `json:"channels"`
	ColorSpace string `json:"color_space"`
}

type Shape struct {
	Type   string     `json:"type"`
	Rect   [4]float64 `json:"rect,omitempty"`
	Radius float64    `json:"radius,omitempty"`
}

type Layer struct {
	ID                string     `json:"id"`
	Name              string     `json:"name"`
	Kind              string     `json:"kind"`
	ParentID          string     `json:"parent_id,omitempty"`
	Visible           bool       `json:"visible"`
	InFrame           int        `json:"in_frame"`
	OutFrame          int        `json:"out_frame"`
	VisibleUntilFrame int        `json:"visible_until_frame"`
	Opacity           float64    `json:"opacity"`
	Alpha             float64    `json:"alpha"`
	Color             [3]float64 `json:"color"`
	ColorEncoding     string     `json:"color_encoding"`
	AlphaMode         string     `json:"alpha_mode"`
	Shape             Shape      `json:"shape"`
	SourceVersion     int        `json:"source_version"`
}

func (l Layer) clone() Layer {
	out := l
	out.Color = l.Color
	out.Shape = l.Shape
	return out
}

func (l Layer) Validate() error {
	if strings.TrimSpace(l.ID) == "" {
		return layerError("", "layer id is required")
	}
	if strings.TrimSpace(l.Name) == "" {
		return layerError(l.ID, "layer name is required")
	}
	if l.Kind != "background" && l.Kind != "prop" && l.Kind != "character" && l.Kind != "group" {
		return layerError(l.ID, "unsupported layer kind %q", l.Kind)
	}
	if l.Opacity < 0 || l.Opacity > 1 || math.IsNaN(l.Opacity) || math.IsInf(l.Opacity, 0) {
		return paraError(-1, "layer %s opacity %.6f is outside [0,1]", l.ID, l.Opacity)
	}
	if l.Alpha < 0 || l.Alpha > 1 || math.IsNaN(l.Alpha) || math.IsInf(l.Alpha, 0) {
		return paraError(-1, "layer %s alpha %.6f is outside [0,1]", l.ID, l.Alpha)
	}
	for i, c := range l.Color {
		if c < 0 || c > 1 || math.IsNaN(c) || math.IsInf(c, 0) {
			return paraError(-1, "layer %s color[%d] %.6f is outside [0,1]", l.ID, i, c)
		}
	}
	if l.InFrame < 0 || l.OutFrame < l.InFrame {
		return paraError(-1, "layer %s has invalid frame range [%d,%d)", l.ID, l.InFrame, l.OutFrame)
	}
	if l.VisibleUntilFrame < l.InFrame {
		return paraError(-1, "layer %s visible_until_frame %d is before in_frame %d", l.ID, l.VisibleUntilFrame, l.InFrame)
	}
	if l.ColorEncoding != ColorSRGB && l.ColorEncoding != ColorLinear {
		return paraError(-1, "layer %s has unsupported color encoding %q", l.ID, l.ColorEncoding)
	}
	if l.AlphaMode != AlphaPremultiplied && l.AlphaMode != AlphaStraight {
		return paraError(-1, "layer %s has unsupported alpha mode %q", l.ID, l.AlphaMode)
	}
	if err := l.Shape.Validate(l.ID); err != nil {
		return err
	}
	return nil
}

func (s Shape) Validate(layerID string) error {
	switch s.Type {
	case ShapeFull:
		return nil
	case ShapeRect, ShapeEllipse:
		if s.Rect[2] <= 0 || s.Rect[3] <= 0 {
			return paraError(-1, "layer %s shape has non-positive bounds", layerID)
		}
		return nil
	default:
		return paraError(-1, "layer %s has unsupported shape %q", layerID, s.Type)
	}
}

type SourceRef struct {
	Path    string `json:"path"`
	Version int    `json:"version"`
	Format  string `json:"format"`
}

// Model is the canonical, mutable scene description. Stack is bottom-to-top.
type Model struct {
	Name             string             `json:"name"`
	SourceVersion    int                `json:"source_version"`
	SourceVersions   []int              `json:"source_versions"`
	Sources          []SourceRef        `json:"sources,omitempty"`
	Canvas           Canvas             `json:"canvas"`
	TimeBase         TimeBase           `json:"time_base"`
	SourceTimeBases  []TimeBase         `json:"source_time_bases,omitempty"`
	Layers           []Layer            `json:"layers"`
	Stack            []string           `json:"stack"`
	FrameCount       int                `json:"frame_count"`
	Instructions     []FrameInstruction `json:"instructions,omitempty"`
	modelFingerprint string
}

type LayerOverride struct {
	Visible *bool       `json:"visible,omitempty"`
	Opacity *float64    `json:"opacity,omitempty"`
	Alpha   *float64    `json:"alpha,omitempty"`
	Color   *[3]float64 `json:"color,omitempty"`
}

type FrameInstruction struct {
	Frame  int                      `json:"frame"`
	Layers map[string]LayerOverride `json:"layers,omitempty"`
}

func (m *Model) Layer(id string) (*Layer, bool) {
	for i := range m.Layers {
		if m.Layers[i].ID == id {
			return &m.Layers[i], true
		}
	}
	return nil, false
}

func (m *Model) Clone() *Model {
	if m == nil {
		return nil
	}
	out := *m
	out.SourceVersions = append([]int(nil), m.SourceVersions...)
	out.Sources = append([]SourceRef(nil), m.Sources...)
	out.SourceTimeBases = append([]TimeBase(nil), m.SourceTimeBases...)
	out.Layers = make([]Layer, len(m.Layers))
	for i, layer := range m.Layers {
		out.Layers[i] = layer.clone()
	}
	out.Stack = append([]string(nil), m.Stack...)
	out.Instructions = make([]FrameInstruction, len(m.Instructions))
	for i, ins := range m.Instructions {
		out.Instructions[i] = FrameInstruction{Frame: ins.Frame, Layers: map[string]LayerOverride{}}
		for id, ov := range ins.Layers {
			cp := ov
			if ov.Color != nil {
				c := *ov.Color
				cp.Color = &c
			}
			out.Instructions[i].Layers[id] = cp
		}
	}
	return &out
}

func (m *Model) Normalize() {
	if m == nil {
		return
	}
	m.SourceVersions = uniqueInts(m.SourceVersions)
	sort.Ints(m.SourceVersions)
	if len(m.SourceVersions) > 0 {
		m.SourceVersion = m.SourceVersions[len(m.SourceVersions)-1]
	}
	if !sort.SliceIsSorted(m.Layers, func(i, j int) bool { return m.Layers[i].ID < m.Layers[j].ID }) {
		// Stack defines rendering order; layer records are normalized by ID so
		// JSON snapshots and hashes never depend on map iteration or input order.
		sort.Slice(m.Layers, func(i, j int) bool { return m.Layers[i].ID < m.Layers[j].ID })
	}
	if m.Canvas.ColorSpace == "" {
		m.Canvas.ColorSpace = ColorLinear
	}
	if m.TimeBase.FramePeriod.Den <= 0 {
		m.TimeBase.FramePeriod = NewRational(1, 24)
		m.TimeBase.Unit = "second"
	}
	m.TimeBase.FramePeriod = m.TimeBase.FramePeriod.Reduced()
}

func uniqueInts(in []int) []int {
	seen := map[int]bool{}
	out := make([]int, 0, len(in))
	for _, v := range in {
		if !seen[v] {
			seen[v] = true
			out = append(out, v)
		}
	}
	return out
}

func (m *Model) Validate() error {
	if m == nil {
		return paraError(-1, "model is nil")
	}
	if m.Canvas.Width <= 0 || m.Canvas.Height <= 0 {
		return paraError(-1, "canvas dimensions must be positive")
	}
	if m.Canvas.Channels != 3 && m.Canvas.Channels != 4 {
		return paraError(-1, "channels must be 3 or 4")
	}
	if m.Canvas.ColorSpace != ColorLinear {
		return paraError(-1, "working color space must be %s", ColorLinear)
	}
	if m.TimeBase.FramePeriod.Num <= 0 || m.TimeBase.FramePeriod.Den <= 0 {
		return paraError(-1, "time base must be positive")
	}
	if m.FrameCount <= 0 {
		return paraError(-1, "frame_count must be positive")
	}
	if len(m.Stack) == 0 {
		return layerError("", "layer stack is empty")
	}
	byID := make(map[string]Layer, len(m.Layers))
	for _, layer := range m.Layers {
		if _, exists := byID[layer.ID]; exists {
			return layerError(layer.ID, "duplicate layer id")
		}
		if err := layer.Validate(); err != nil {
			return err
		}
		byID[layer.ID] = layer
	}
	for _, id := range m.Stack {
		if _, ok := byID[id]; !ok {
			return layerError(id, "stack references missing layer")
		}
	}
	seen := map[string]bool{}
	for _, id := range m.Stack {
		if seen[id] {
			return layerError(id, "layer appears more than once in stack")
		}
		seen[id] = true
	}
	for id := range byID {
		if err := validateParentChain(id, byID); err != nil {
			return err
		}
	}
	for _, ins := range m.Instructions {
		if ins.Frame < 0 || ins.Frame >= m.FrameCount {
			return paraError(-1, "instruction frame %d is outside [0,%d)", ins.Frame, m.FrameCount)
		}
		for id := range ins.Layers {
			if _, ok := byID[id]; !ok {
				return layerError(id, "instruction references missing layer")
			}
		}
	}
	return nil
}

func validateParentChain(id string, layers map[string]Layer) error {
	seen := map[string]bool{}
	cur := id
	for cur != "" {
		if seen[cur] {
			return layerError(cur, "layer hierarchy contains a cycle")
		}
		seen[cur] = true
		layer, ok := layers[cur]
		if !ok {
			return layerError(cur, "layer hierarchy references missing parent")
		}
		cur = layer.ParentID
	}
	return nil
}

func instructionForFrame(model *Model, frame int) FrameInstruction {
	for _, ins := range model.Instructions {
		if ins.Frame == frame {
			return ins
		}
	}
	return FrameInstruction{Frame: frame}
}

func layerAtFrame(layer Layer, frame int, ins FrameInstruction) (Layer, bool) {
	out := layer
	if frame < out.InFrame || frame >= out.OutFrame || frame >= out.VisibleUntilFrame {
		return out, false
	}
	if ov, ok := ins.Layers[layer.ID]; ok {
		if ov.Visible != nil {
			out.Visible = *ov.Visible
		}
		if ov.Opacity != nil {
			out.Opacity = *ov.Opacity
		}
		if ov.Alpha != nil {
			out.Alpha = *ov.Alpha
		}
		if ov.Color != nil {
			out.Color = *ov.Color
		}
	}
	return out, out.Visible
}

func (m *Model) layerAffectedFrames(layer Layer) map[int]struct{} {
	affected := map[int]struct{}{}
	start := clamp(layer.InFrame, 0, m.FrameCount)
	end := clamp(layer.VisibleUntilFrame, 0, m.FrameCount)
	if layer.OutFrame < end {
		end = clamp(layer.OutFrame, 0, m.FrameCount)
	}
	for frame := start; frame < end; frame++ {
		affected[frame] = struct{}{}
	}
	for _, ins := range m.Instructions {
		if _, ok := ins.Layers[layer.ID]; ok && ins.Frame >= 0 && ins.Frame < m.FrameCount {
			affected[ins.Frame] = struct{}{}
		}
	}
	return affected
}

func clamp(v, lo, hi int) int {
	if v < lo {
		return lo
	}
	if v > hi {
		return hi
	}
	return v
}

func (m *Model) String() string {
	return fmt.Sprintf("%s v%d %dx%d/%dch %d layers %d frames", m.Name, m.SourceVersion, m.Canvas.Width, m.Canvas.Height, m.Canvas.Channels, len(m.Layers), m.FrameCount)
}
