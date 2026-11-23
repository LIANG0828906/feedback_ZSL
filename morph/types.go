package morph

import "math"

const (
	SceneFormat      = "pathmorph.scene.v1"
	TruthFormat      = "pathmorph.truth.v1"
	CheckpointFormat = "pathmorph.checkpoint.v1"
	AlgorithmVersion = "pathmorph-kernel-1.0.0"
)

// Vec2 is a point in world coordinates.
type Vec2 struct {
	X float64 `json:"x"`
	Y float64 `json:"y"`
}

func (v Vec2) Add(o Vec2) Vec2         { return Vec2{v.X + o.X, v.Y + o.Y} }
func (v Vec2) Sub(o Vec2) Vec2         { return Vec2{v.X - o.X, v.Y - o.Y} }
func (v Vec2) Mul(s float64) Vec2      { return Vec2{v.X * s, v.Y * s} }
func (v Vec2) Dot(o Vec2) float64      { return v.X*o.X + v.Y*o.Y }
func (v Vec2) Cross(o Vec2) float64    { return v.X*o.Y - v.Y*o.X }
func (v Vec2) Len() float64            { return math.Hypot(v.X, v.Y) }
func (v Vec2) Distance(o Vec2) float64 { return v.Sub(o).Len() }

func (v Vec2) Normalized() Vec2 {
	l := v.Len()
	if l == 0 {
		return Vec2{}
	}
	return Vec2{v.X / l, v.Y / l}
}

// BBox is an axis-aligned world-space rectangle.
type BBox struct {
	Min Vec2 `json:"min"`
	Max Vec2 `json:"max"`
}

func EmptyBBox() BBox {
	return BBox{Min: Vec2{math.Inf(1), math.Inf(1)}, Max: Vec2{math.Inf(-1), math.Inf(-1)}}
}

func (b BBox) IsEmpty() bool { return b.Min.X > b.Max.X || b.Min.Y > b.Max.Y }

func (b BBox) Include(p Vec2) BBox {
	if b.IsEmpty() {
		return BBox{Min: p, Max: p}
	}
	if p.X < b.Min.X {
		b.Min.X = p.X
	}
	if p.Y < b.Min.Y {
		b.Min.Y = p.Y
	}
	if p.X > b.Max.X {
		b.Max.X = p.X
	}
	if p.Y > b.Max.Y {
		b.Max.Y = p.Y
	}
	return b
}

func (b BBox) Union(o BBox) BBox {
	if b.IsEmpty() {
		return o
	}
	if o.IsEmpty() {
		return b
	}
	return BBox{
		Min: Vec2{math.Min(b.Min.X, o.Min.X), math.Min(b.Min.Y, o.Min.Y)},
		Max: Vec2{math.Max(b.Max.X, o.Max.X), math.Max(b.Max.Y, o.Max.Y)},
	}
}

func (b BBox) Expand(d float64) BBox {
	if b.IsEmpty() {
		return b
	}
	return BBox{Min: Vec2{b.Min.X - d, b.Min.Y - d}, Max: Vec2{b.Max.X + d, b.Max.Y + d}}
}

func (b BBox) Width() float64  { return b.Max.X - b.Min.X }
func (b BBox) Height() float64 { return b.Max.Y - b.Min.Y }

func (b BBox) Center() Vec2 {
	return Vec2{(b.Min.X + b.Max.X) * 0.5, (b.Min.Y + b.Max.Y) * 0.5}
}

// Segment is one quadratic (Q) or cubic (C) Bezier segment.
// Q uses P0, P1 and P2. C uses P0, P1, P2 and P3.
type Segment struct {
	Kind string `json:"kind"`
	P0   Vec2   `json:"p0"`
	P1   Vec2   `json:"p1"`
	P2   Vec2   `json:"p2"`
	P3   *Vec2  `json:"p3,omitempty"`
}

func (s Segment) End() Vec2 {
	if s.Kind == "C" && s.P3 != nil {
		return *s.P3
	}
	return s.P2
}

func (s Segment) Degree() int {
	if s.Kind == "C" {
		return 3
	}
	return 2
}

func (s Segment) Points() []Vec2 {
	if s.Kind == "C" && s.P3 != nil {
		return []Vec2{s.P0, s.P1, s.P2, *s.P3}
	}
	return []Vec2{s.P0, s.P1, s.P2}
}

func VecPtr(v Vec2) *Vec2 { return &v }

// Keyframe stores the complete closed contour at one animation time.
type Keyframe struct {
	Time     float64   `json:"time"`
	Segments []Segment `json:"segments"`
}

type FillStyle struct {
	Color [3]float64 `json:"color"`
}

type StrokeStyle struct {
	Color      [3]float64 `json:"color"`
	Width      float64    `json:"width"`
	Join       string     `json:"join"`
	MiterLimit float64    `json:"miter_limit,omitempty"`
}

type Path struct {
	ID        string       `json:"id"`
	Fill      *FillStyle   `json:"fill,omitempty"`
	Stroke    *StrokeStyle `json:"stroke,omitempty"`
	Opacity   float64      `json:"opacity"`
	BlendMode string       `json:"blend_mode"`
	FillRule  string       `json:"fill_rule,omitempty"`
	Keyframes []Keyframe   `json:"keyframes"`
}

type RenderDefaults struct {
	Scale          float64 `json:"scale"`
	PaddingPixels  int     `json:"padding_pixels"`
	MaxFramePixels int     `json:"max_frame_pixels"`
}

type Scene struct {
	Format      string         `json:"format"`
	Version     int            `json:"version"`
	ID          string         `json:"id"`
	Seed        int64          `json:"seed"`
	Quantum     float64        `json:"quantum"`
	CoordMin    float64        `json:"coord_min"`
	CoordMax    float64        `json:"coord_max"`
	Render      RenderDefaults `json:"render_defaults"`
	SampleTimes []float64      `json:"sample_times"`
	Paths       []Path         `json:"paths"`
}

type CurveSample struct {
	Segment int     `json:"segment"`
	T       float64 `json:"t"`
	Point   Vec2    `json:"point"`
}

type PathTruth struct {
	PathID                string        `json:"path_id"`
	CurveSamples          []CurveSample `json:"curve_samples"`
	ContourArea           float64       `json:"contour_area"`
	FillArea              float64       `json:"fill_area"`
	StrokeArea            float64       `json:"stroke_area"`
	ReferenceCoverageArea float64       `json:"reference_coverage_area"`
	GeometryBBox          BBox          `json:"geometry_bbox"`
	Closed                bool          `json:"closed"`
	ClosureCheckpoints    []Vec2        `json:"closure_checkpoints"`
}

type TruthSample struct {
	Index               int         `json:"index"`
	Time                float64     `json:"time"`
	MidpointOfKeyframes bool        `json:"midpoint_of_keyframes"`
	MidpointLower       float64     `json:"midpoint_lower,omitempty"`
	MidpointUpper       float64     `json:"midpoint_upper,omitempty"`
	FrameBBox           BBox        `json:"frame_bbox"`
	RenderBBox          BBox        `json:"render_bbox"`
	Width               int         `json:"width"`
	Height              int         `json:"height"`
	Paths               []PathTruth `json:"paths"`
}

type Truth struct {
	Format    string         `json:"format"`
	Version   int            `json:"version"`
	SceneID   string         `json:"scene_id"`
	Seed      int64          `json:"seed"`
	Algorithm string         `json:"algorithm"`
	Render    RenderDefaults `json:"render_defaults"`
	Samples   []TruthSample  `json:"samples"`
}

// EvalConfig controls keyframe evaluation and midpoint flattening.
type EvalConfig struct {
	Interpolation string  `json:"interpolation"`
	Scale         float64 `json:"scale"`
	MidTolerance  float64 `json:"mid_tolerance"`
}

// RenderConfig controls antialiasing and frame size derivation.
type RenderConfig struct {
	Grid           int `json:"grid"`
	PaddingPixels  int `json:"padding_pixels"`
	MaxFramePixels int `json:"max_frame_pixels"`
}

// AdaptiveConfig contains the fine-mode baseline and resource policy.
type AdaptiveConfig struct {
	MemoryBudgetBytes  int64   `json:"memory_budget_bytes"`
	WorkThreshold      int64   `json:"work_threshold"`
	ForceFine          bool    `json:"force_fine"`
	BaseMidTolerance   float64 `json:"base_mid_tolerance"`
	BaseFrameTolerance float64 `json:"base_frame_tolerance"`
	BaseGrid           int     `json:"base_grid"`
	BaseChunkSize      int     `json:"base_chunk_size"`
}

type RuntimeConfig struct {
	Workers             int   `json:"workers"`
	QueueSize           int   `json:"queue_size"`
	PerThreadPixelBytes int64 `json:"per_thread_pixel_bytes"`
	MaxChunkPaths       int   `json:"max_chunk_paths"`
	StopAfterCommits    int   `json:"stop_after_commits,omitempty"`
}

type ConsistencyTolerance struct {
	RelativeArea float64 `json:"relative_area"`
	BBoxIoU      float64 `json:"bbox_iou"`
	MeanCoverage float64 `json:"mean_coverage"`
}

type Config struct {
	Eval        EvalConfig           `json:"eval"`
	Render      RenderConfig         `json:"render"`
	Adaptive    AdaptiveConfig       `json:"adaptive"`
	Runtime     RuntimeConfig        `json:"runtime"`
	Consistency ConsistencyTolerance `json:"consistency"`
	Checkpoint  string               `json:"checkpoint"`
}

func DefaultConfig() Config {
	return Config{
		Eval: EvalConfig{
			Interpolation: "linear",
			Scale:         12,
			MidTolerance:  0.02,
		},
		Render: RenderConfig{
			Grid:           4,
			PaddingPixels:  4,
			MaxFramePixels: 2048,
		},
		Adaptive: AdaptiveConfig{
			MemoryBudgetBytes:  256 << 20,
			WorkThreshold:      250000,
			BaseMidTolerance:   0.02,
			BaseFrameTolerance: 0.20,
			BaseGrid:           4,
			BaseChunkSize:      32,
		},
		Runtime: RuntimeConfig{
			Workers:             4,
			QueueSize:           16,
			PerThreadPixelBytes: 64 << 20,
			MaxChunkPaths:       32,
		},
		Consistency: ConsistencyTolerance{
			RelativeArea: 0.04,
			BBoxIoU:      0.94,
			MeanCoverage: 0.035,
		},
	}
}

type Quality struct {
	Mode           string   `json:"mode"`
	MidTolerance   float64  `json:"mid_tolerance"`
	FrameTolerance float64  `json:"frame_tolerance"`
	Grid           int      `json:"grid"`
	ChunkSize      int      `json:"chunk_size"`
	MemoryBudget   int64    `json:"memory_budget_bytes"`
	EstimatedBytes int64    `json:"estimated_bytes"`
	WorkUnits      int64    `json:"work_units"`
	ForcedFine     bool     `json:"forced_fine"`
	Reasons        []string `json:"reasons"`
}

type Stage string

const (
	StageFlatten   Stage = "flatten"
	StageCoverage  Stage = "coverage"
	StageComposite Stage = "composite"
)

type State string

const (
	StatePending    State = "pending"
	StateProcessing State = "processing"
	StateCommitted  State = "committed"
	StateFailed     State = "failed"
)

// PixelRect uses half-open bounds in frame pixel coordinates.
type PixelRect struct {
	X0 int `json:"x0"`
	Y0 int `json:"y0"`
	X1 int `json:"x1"`
	Y1 int `json:"y1"`
}

func (r PixelRect) Empty() bool { return r.X1 <= r.X0 || r.Y1 <= r.Y0 }
func (r PixelRect) Width() int  { return r.X1 - r.X0 }
func (r PixelRect) Height() int { return r.Y1 - r.Y0 }
func (r PixelRect) Area() int   { return r.Width() * r.Height() }

func (r PixelRect) Intersect(o PixelRect) PixelRect {
	out := PixelRect{
		X0: maxInt(r.X0, o.X0),
		Y0: maxInt(r.Y0, o.Y0),
		X1: minInt(r.X1, o.X1),
		Y1: minInt(r.Y1, o.Y1),
	}
	if out.Empty() {
		return PixelRect{}
	}
	return out
}

func (r PixelRect) Union(o PixelRect) PixelRect {
	if r.Empty() {
		return o
	}
	if o.Empty() {
		return r
	}
	return PixelRect{
		X0: minInt(r.X0, o.X0),
		Y0: minInt(r.Y0, o.Y0),
		X1: maxInt(r.X1, o.X1),
		Y1: maxInt(r.Y1, o.Y1),
	}
}

func (r PixelRect) IoU(o PixelRect) float64 {
	if r.Empty() && o.Empty() {
		return 1
	}
	inter := r.Intersect(o)
	if inter.Empty() {
		return 0
	}
	union := r.Area() + o.Area() - inter.Area()
	if union <= 0 {
		return 1
	}
	return float64(inter.Area()) / float64(union)
}

func minInt(a, b int) int {
	if a < b {
		return a
	}
	return b
}
func maxInt(a, b int) int {
	if a > b {
		return a
	}
	return b
}
