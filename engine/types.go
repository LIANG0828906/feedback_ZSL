package engine

import (
	"encoding/json"
	"fmt"
	"strings"
)

const (
	// CoordScale is the fixed-point denominator for all internal coordinates.
	CoordScale int64 = 1 << 10
	// TimeTicksPerMillisecond is the fixed-point denominator for internal time.
	TimeTicksPerMillisecond int64 = 1000
	// SampleFPS is deliberately chosen so frame <-> millisecond conversion is exact.
	SampleFPS int64 = 100
	// TimeTicksPerFrame is the internal time of one sample frame.
	TimeTicksPerFrame int64 = TimeTicksPerMillisecond * 1000 / SampleFPS
)

type Version string

const (
	Version1 Version = "1.0"
	Version2 Version = "2.0"
	Version3 Version = "3.0"
)

type Vec2 struct {
	X int64 `json:"x"`
	Y int64 `json:"y"`
}

func (p Vec2) Add(q Vec2) Vec2 { return Vec2{X: p.X + q.X, Y: p.Y + q.Y} }
func (p Vec2) Sub(q Vec2) Vec2 { return Vec2{X: p.X - q.X, Y: p.Y - q.Y} }
func (p Vec2) MulDiv(m, d int64) Vec2 {
	return Vec2{X: mulDivRound(p.X, m, d), Y: mulDivRound(p.Y, m, d)}
}

type Rect struct {
	MinX int64 `json:"minX"`
	MinY int64 `json:"minY"`
	MaxX int64 `json:"maxX"`
	MaxY int64 `json:"maxY"`
}

func (r Rect) Width() int64  { return r.MaxX - r.MinX }
func (r Rect) Height() int64 { return r.MaxY - r.MinY }
func (r Rect) Empty() bool   { return r.MaxX <= r.MinX || r.MaxY <= r.MinY }

func (r Rect) Union(q Rect) Rect {
	if r.Empty() {
		return q
	}
	if q.Empty() {
		return r
	}
	return Rect{
		MinX: min64(r.MinX, q.MinX),
		MinY: min64(r.MinY, q.MinY),
		MaxX: max64(r.MaxX, q.MaxX),
		MaxY: max64(r.MaxY, q.MaxY),
	}
}

func (r Rect) Expand(amount int64) Rect {
	return Rect{MinX: r.MinX - amount, MinY: r.MinY - amount, MaxX: r.MaxX + amount, MaxY: r.MaxY + amount}
}

type Color struct {
	R uint8 `json:"r"`
	G uint8 `json:"g"`
	B uint8 `json:"b"`
	A uint8 `json:"a"`
}

type SegmentKind string

const (
	Quadratic SegmentKind = "quadratic"
	Cubic     SegmentKind = "cubic"
)

// Segment stores one closed-contour segment. C2 is ignored for quadratic curves.
type Segment struct {
	Kind  SegmentKind `json:"kind"`
	Start Vec2        `json:"start"`
	C1    Vec2        `json:"c1"`
	C2    Vec2        `json:"c2,omitempty"`
	End   Vec2        `json:"end"`
}

type Keyframe struct {
	Time     int64     `json:"time"`
	Segments []Segment `json:"segments"`
}

type JoinKind string

const (
	JoinRound JoinKind = "round"
	JoinBevel JoinKind = "bevel"
	JoinMiter JoinKind = "miter"
)

type BlendMode string

const (
	BlendSourceOver BlendMode = "source-over"
	BlendMultiply   BlendMode = "multiply"
	BlendScreen     BlendMode = "screen"
	BlendReplace    BlendMode = "replace"
)

type Path struct {
	ID          string     `json:"id"`
	HasFill     bool       `json:"hasFill"`
	HasStroke   bool       `json:"hasStroke"`
	Fill        Color      `json:"fill"`
	StrokeColor Color      `json:"strokeColor"`
	StrokeWidth int64      `json:"strokeWidth"` // Q10 logical pixels
	LineJoin    JoinKind   `json:"lineJoin"`
	Blend       BlendMode  `json:"blend"`
	FillRule    string     `json:"fillRule"`
	Opacity     uint8      `json:"opacity"`
	Keyframes   []Keyframe `json:"keyframes"`
}

type InterpOptions struct {
	Mode          string `json:"mode"`
	PrecisionBits int    `json:"precisionBits"`
}

type Scene struct {
	ID               string        `json:"id"`
	SourceVersion    Version       `json:"sourceVersion"`
	TimeUnit         string        `json:"timeUnit"`
	Encoding         string        `json:"encoding"`
	Rounding         string        `json:"rounding"`
	Interpolation    InterpOptions `json:"interpolation"`
	Tolerance        int64         `json:"tolerance"` // Q10
	Subsamples       int           `json:"subsamples"`
	Scale            int64         `json:"scale"` // Q10; 1024 means one logical pixel per pixel
	Paths            []Path        `json:"paths"`
	Samples          []int64       `json:"samples"` // internal ticks
	BlendVersion     string        `json:"blendVersion"`
	AlgorithmVersion string        `json:"algorithmVersion"`
}

func (s *Scene) Clone() *Scene {
	if s == nil {
		return nil
	}
	b, _ := json.Marshal(s)
	var out Scene
	_ = json.Unmarshal(b, &out)
	return &out
}

func (s *Scene) Bounds() Rect {
	if s == nil {
		return Rect{}
	}
	var points []Vec2
	for _, p := range s.Paths {
		for _, k := range p.Keyframes {
			for _, seg := range k.Segments {
				for _, pt := range []Vec2{seg.Start, seg.C1, seg.End} {
					points = append(points, pt)
				}
				if seg.Kind == Cubic {
					points = append(points, seg.C2)
				}
			}
		}
	}
	return BoundsOfPoints(points)
}

// BoundsOfPoints returns a tight bounds even when every point is identical.
func BoundsOfPoints(points []Vec2) Rect {
	if len(points) == 0 {
		return Rect{}
	}
	out := Rect{MinX: points[0].X, MinY: points[0].Y, MaxX: points[0].X, MaxY: points[0].Y}
	for _, p := range points[1:] {
		out.MinX = min64(out.MinX, p.X)
		out.MinY = min64(out.MinY, p.Y)
		out.MaxX = max64(out.MaxX, p.X)
		out.MaxY = max64(out.MaxY, p.Y)
	}
	return out
}

func (s *Scene) ValidateSamples() error {
	if len(s.Samples) == 0 {
		return errAt(CodeSchema, "samples", "at least one sample time is required")
	}
	for i, t := range s.Samples {
		if i > 0 && t <= s.Samples[i-1] {
			return &Error{Code: CodeSelf, Path: fmt.Sprintf("samples[%d]", i), Message: "sample times must be strictly increasing"}
		}
	}
	return nil
}

type CurveSample struct {
	Segment int   `json:"segment"`
	T       int64 `json:"tQ30"`
	Point   Vec2  `json:"point"`
}

type TruthPath struct {
	ID                string        `json:"id"`
	Samples           []CurveSample `json:"samples"`
	Area              int64         `json:"areaQ20"`
	Bounds            Rect          `json:"bounds"`
	CoverageRatio     float64       `json:"coverageRatio"`
	CoverageBBoxRatio float64       `json:"coverageBBoxRatio"`
	CurvePointCount   int           `json:"curvePointCount"`
}

type TruthFrame struct {
	Time     int64       `json:"timeUs"`
	TimeMs   int64       `json:"timeMs"`
	Bounds   Rect        `json:"bounds"`
	Paths    []TruthPath `json:"paths"`
	Checksum string      `json:"checksum"`
}

type TruthDocument struct {
	SceneID        string       `json:"sceneId"`
	CoordinateUnit string       `json:"coordinateUnit"`
	TimeUnit       string       `json:"timeUnit"`
	FPS            int64        `json:"fps"`
	Samples        []TruthFrame `json:"samples"`
}

func (d *TruthDocument) FrameAt(time int64) *TruthFrame {
	for i := range d.Samples {
		if d.Samples[i].Time == time {
			return &d.Samples[i]
		}
	}
	return nil
}

type Defaults struct {
	Interpolation InterpOptions
	Tolerance     int64
	Subsamples    int
	Scale         int64
}

func StandardDefaults() Defaults {
	return Defaults{
		Interpolation: InterpOptions{Mode: "linear", PrecisionBits: 12},
		Tolerance:     64,
		Subsamples:    4,
		Scale:         CoordScale,
	}
}

func (d Defaults) Apply(s *Scene) {
	if s.Interpolation.Mode == "" {
		s.Interpolation = d.Interpolation
	}
	if s.Interpolation.PrecisionBits == 0 {
		s.Interpolation.PrecisionBits = d.Interpolation.PrecisionBits
	}
	if s.Tolerance == 0 {
		s.Tolerance = d.Tolerance
	}
	if s.Subsamples == 0 {
		s.Subsamples = d.Subsamples
	}
	if s.Scale == 0 {
		s.Scale = d.Scale
	}
	if s.BlendVersion == "" {
		s.BlendVersion = "blend-v1"
	}
	if s.AlgorithmVersion == "" {
		s.AlgorithmVersion = "algo-v1"
	}
}

func normalizedVersion(raw any) (Version, bool) {
	s := strings.TrimSpace(fmt.Sprint(raw))
	switch {
	case s == "1" || strings.HasPrefix(s, "1."):
		return Version1, true
	case s == "2" || strings.HasPrefix(s, "2."):
		return Version2, true
	case s == "3" || strings.HasPrefix(s, "3."):
		return Version3, true
	default:
		return "", false
	}
}

func min64(a, b int64) int64 {
	if a < b {
		return a
	}
	return b
}

func max64(a, b int64) int64 {
	if a > b {
		return a
	}
	return b
}
