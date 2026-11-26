package vpf

import "time"

const (
	SchemaV1 = 1
	SchemaV2 = 2
	SchemaV3 = 3

	OpacityScale = 10000
	ColorMax     = 255

	InternalTimeUnit  = "nanosecond"
	InternalColorUnit = "straight_alpha_8bit"
	AlgorithmVersion  = "vpf-integer-stack-1.0.0"

	// One v1 tick is 1/2 millisecond, i.e. 500,000 ns.
	TickNanoseconds = int64(500_000)

	MillisecondNanoseconds = int64(time.Millisecond)
	MicrosecondNanoseconds = int64(time.Microsecond)
)

type BlendMode string

const (
	BlendNormal   BlendMode = "normal"
	BlendAdd      BlendMode = "add"
	BlendMultiply BlendMode = "multiply"
)

type LayerKind string

const (
	KindGround LayerKind = "ground"
	KindRipple LayerKind = "ripple_mask"
	KindSolid  LayerKind = "solid"
	KindGuide  LayerKind = "guide"
)

type AlphaConvention string

const (
	AlphaStraight      AlphaConvention = "straight"
	AlphaPremultiplied AlphaConvention = "premultiplied"
)

type Point struct {
	X int `json:"x"`
	Y int `json:"y"`
}

type Rect struct {
	X int `json:"x"`
	Y int `json:"y"`
	W int `json:"w"`
	H int `json:"h"`
}

func (r Rect) Contains(x, y int) bool {
	return x >= r.X && y >= r.Y && x < r.X+r.W && y < r.Y+r.H
}

func (r Rect) Intersects(other Rect) bool {
	return r.X < other.X+other.W && r.X+r.W > other.X &&
		r.Y < other.Y+other.H && r.Y+r.H > other.Y
}

func (r Rect) Intersection(other Rect) Rect {
	x0 := maxInt(r.X, other.X)
	y0 := maxInt(r.Y, other.Y)
	x1 := minInt(r.X+r.W, other.X+other.W)
	y1 := minInt(r.Y+r.H, other.Y+other.H)
	if x1 <= x0 || y1 <= y0 {
		return Rect{}
	}
	return Rect{X: x0, Y: y0, W: x1 - x0, H: y1 - y0}
}

type RGBA struct {
	R uint8 `json:"r"`
	G uint8 `json:"g"`
	B uint8 `json:"b"`
	A uint8 `json:"a"`
}

type RippleSource struct {
	ID           string `json:"id"`
	Center       Point  `json:"center"`
	Radius       int    `json:"radius"`
	DecayBP      int    `json:"decay_bp"`
	AmplitudeBP  int    `json:"amplitude_bp"`
	ParamVersion int    `json:"param_version,omitempty"`
}

type Layer struct {
	ID              string          `json:"id"`
	Name            string          `json:"name"`
	Kind            LayerKind       `json:"kind"`
	Rect            Rect            `json:"rect"`
	Visible         bool            `json:"visible"`
	OpacityBP       int             `json:"opacity_bp"`
	Blend           BlendMode       `json:"blend"`
	Color           RGBA            `json:"color"`
	ContentSummary  string          `json:"content_summary"`
	ContentHash     string          `json:"content_hash"`
	ParamVersion    int             `json:"param_version"`
	Ripples         []RippleSource  `json:"ripples,omitempty"`
	SourcePath      string          `json:"source_path,omitempty"`
	AlphaConvention AlphaConvention `json:"alpha_convention,omitempty"`
}

type NodeKind string

const (
	NodeGroup NodeKind = "group"
	NodeLayer NodeKind = "layer"
)

// Node is the retained hierarchical expression. Flat schemas produce one root.
type Node struct {
	ID         string   `json:"id"`
	Kind       NodeKind `json:"kind"`
	Label      string   `json:"label,omitempty"`
	Visible    bool     `json:"visible"`
	LayerID    string   `json:"layer_id,omitempty"`
	Children   []Node   `json:"children,omitempty"`
	ParentHint string   `json:"parent_hint,omitempty"`
}

type ResolvedLayer struct {
	Layer            Layer  `json:"layer"`
	SourcePath       string `json:"source_path"`
	TreePath         string `json:"tree_path"`
	Depth            int    `json:"depth"`
	EffectiveVisible bool   `json:"effective_visible"`
}

type Frame struct {
	Index int    `json:"index"`
	AtNS  int64  `json:"at_ns"`
	Label string `json:"label"`
}

type Instruction struct {
	Order       int    `json:"order"`
	AtNS        int64  `json:"at_ns"`
	Op          string `json:"op"`
	LayerID     string `json:"layer_id"`
	SourceID    string `json:"source_id,omitempty"`
	Radius      int    `json:"radius,omitempty"`
	AmplitudeBP int    `json:"amplitude_bp,omitempty"`
}

type FieldTrace struct {
	SourceVersion int    `json:"source_version"`
	SourcePath    string `json:"source_path"`
	TargetPath    string `json:"target_path"`
	Transform     string `json:"transform"`
	OldValue      any    `json:"old_value,omitempty"`
	NewValue      any    `json:"new_value,omitempty"`
	DefaultValue  any    `json:"default_value,omitempty"`
	Note          string `json:"note,omitempty"`
}

type TruncationRecord struct {
	Field       string `json:"field"`
	Operation   string `json:"operation"`
	Input       int64  `json:"input"`
	Operand     int64  `json:"operand"`
	Denominator int64  `json:"denominator"`
	Output      int64  `json:"output"`
	Remainder   int64  `json:"remainder"`
	Direction   string `json:"direction"`
	At          string `json:"at"`
}

type TraceBook struct {
	Fields      []FieldTrace       `json:"fields"`
	Truncations []TruncationRecord `json:"truncations,omitempty"`
}

func (t *TraceBook) Add(trace FieldTrace) {
	t.Fields = append(t.Fields, trace)
}

func (t *TraceBook) AddTruncation(record TruncationRecord) {
	t.Truncations = append(t.Truncations, record)
}

// Project is the canonical internal ordered layer-stack model.
type Project struct {
	ProjectID            string          `json:"project_id"`
	Seed                 int64           `json:"seed"`
	SourceVersion        int             `json:"source_version"`
	Width                int             `json:"width"`
	Height               int             `json:"height"`
	Channels             int             `json:"channels"`
	TileWidth            int             `json:"tile_width"`
	TileHeight           int             `json:"tile_height"`
	Frames               []Frame         `json:"frames"`
	Instructions         []Instruction   `json:"instructions,omitempty"`
	Layers               []Layer         `json:"layers"`
	Root                 Node            `json:"root"`
	Resolved             []ResolvedLayer `json:"resolved"`
	Stack                []Layer         `json:"stack"`
	Traces               TraceBook       `json:"traces,omitempty"`
	StackSnapshotVersion int             `json:"stack_snapshot_version"`
	RippleSourceVersion  int             `json:"ripple_source_version"`
}

func (p *Project) LayerByID(id string) (Layer, bool) {
	for _, layer := range p.Layers {
		if layer.ID == id {
			return layer, true
		}
	}
	return Layer{}, false
}

func (p *Project) FrameByIndex(index int) (Frame, bool) {
	for _, frame := range p.Frames {
		if frame.Index == index {
			return frame, true
		}
	}
	return Frame{}, false
}

func (p *Project) TileGrid() (cols, rows int) {
	cols = (p.Width + p.TileWidth - 1) / p.TileWidth
	rows = (p.Height + p.TileHeight - 1) / p.TileHeight
	return cols, rows
}

func (p *Project) TileRect(index int) Rect {
	cols, _ := p.TileGrid()
	col := index % cols
	row := index / cols
	x := col * p.TileWidth
	y := row * p.TileHeight
	w := minInt(p.TileWidth, p.Width-x)
	h := minInt(p.TileHeight, p.Height-y)
	return Rect{X: x, Y: y, W: w, H: h}
}

func (p *Project) ChunkCount() int {
	cols, rows := p.TileGrid()
	return cols * rows
}

func maxInt(a, b int) int {
	if a > b {
		return a
	}
	return b
}

func minInt(a, b int) int {
	if a < b {
		return a
	}
	return b
}
