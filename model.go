package vectorcore

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"runtime"
	"sort"
	"strconv"
	"strings"
)

const (
	// CoordScale is the number of fixed-point coordinate units per logical
	// pixel. Geometry never uses floating-point accumulation internally.
	CoordScale int64 = 4096

	// TicksPerSecond is divisible by 60, 120, 30, 50, 24 and 25 frame rates.
	TicksPerSecond int64 = 36_000_000

	AlgorithmVersion              = "vectorcore-raster/1"
	InterpolationVersion          = "interpolation/1"
	ToleranceVersion              = "flattening/1"
	CompositeVersion              = "composite/1"
	CheckpointSchemaVersion       = "checkpoint/1"
	MaxWorkersHardLimit           = 8
	MaxBufferBytesHardLimit int64 = 256 << 20
)

type Point struct {
	X int64 `json:"x"`
	Y int64 `json:"y"`
}

type SegmentKind string

const (
	SegmentQuad  SegmentKind = "quad"
	SegmentCubic SegmentKind = "cubic"
)

type Segment struct {
	Kind SegmentKind `json:"kind"`
	C1   Point       `json:"c1"`
	C2   Point       `json:"c2"`
	To   Point       `json:"to"`
}

// Contour is implicitly closed. The final segment endpoint normally equals
// Start; loaders and flattening both normalize it to a closed representation.
type Contour struct {
	Start    Point     `json:"start"`
	Segments []Segment `json:"segments"`
}

type Keyframe struct {
	Time    int64   `json:"time"`
	Contour Contour `json:"contour"`
}

type RGBA struct {
	R uint8 `json:"r"`
	G uint8 `json:"g"`
	B uint8 `json:"b"`
	A uint8 `json:"a"`
}

type BlendMode string

const (
	BlendSourceOver BlendMode = "source-over"
	BlendNormal     BlendMode = "normal"
	BlendAdd        BlendMode = "add"
	BlendMultiply   BlendMode = "multiply"
	BlendScreen     BlendMode = "screen"
)

type JoinMode string

const (
	JoinRound JoinMode = "round"
	JoinBevel JoinMode = "bevel"
	JoinMiter JoinMode = "miter"
)

type Paint struct {
	Fill        bool
	StrokeWidth int64
	Join        JoinMode
	Color       RGBA
	Opacity     int // 0..10000, parts per ten-thousand
	Blend       BlendMode
}

type Path struct {
	ID        string
	Paint     Paint
	Keyframes []Keyframe
}

type Scene struct {
	ID            string
	SourceVersion int
	FPS           int64
	Rounding      Rounding
	SampleTimes   []int64
	Paths         []Path
}

type Scale struct {
	Num int64 `json:"num"`
	Den int64 `json:"den"`
}

var UnitScale = Scale{Num: 1, Den: 1}

func (s Scale) Valid() bool { return s.Num > 0 && s.Den > 0 }

func (s Scale) String() string { return fmt.Sprintf("%d/%d", s.Num, s.Den) }

type InterpMode string

const (
	InterpLinear     InterpMode = "linear"
	InterpSmoothStep InterpMode = "smoothstep"
	InterpHold       InterpMode = "hold"
)

type FillRule string

const (
	FillNonZero FillRule = "nonzero"
	FillEvenOdd FillRule = "evenodd"
)

type Config struct {
	Interpolation       InterpMode
	Tolerance           int64
	Scale               Scale
	Grid                int
	FillRule            FillRule
	Margin              int
	MaxWorkers          int
	MaxPixelBufferBytes int64
}

func DefaultConfig() Config {
	workers := runtime.NumCPU()
	if workers > 4 {
		workers = 4
	}
	if workers < 1 {
		workers = 1
	}
	return Config{
		Interpolation:       InterpLinear,
		Tolerance:           256,
		Scale:               UnitScale,
		Grid:                4,
		FillRule:            FillNonZero,
		Margin:              1,
		MaxWorkers:          workers,
		MaxPixelBufferBytes: 64 << 20,
	}
}

func (c Config) Validate() error {
	switch c.Interpolation {
	case InterpLinear, InterpSmoothStep, InterpHold:
	default:
		return &Error{Code: CodeParam, Op: "validate", Field: "interpolation", Keyframe: -1, TimeIndex: -1,
			Err: fmt.Errorf("unsupported interpolation %q", c.Interpolation)}
	}
	if c.Tolerance < 1 || c.Tolerance > 1<<20 {
		return &Error{Code: CodeParam, Op: "validate", Field: "tolerance", Keyframe: -1, TimeIndex: -1,
			Err: fmt.Errorf("must be in [1,%d]", 1<<20)}
	}
	if !c.Scale.Valid() || c.Scale.Num > 64 || c.Scale.Den > 64 {
		return &Error{Code: CodeParam, Op: "validate", Field: "scale", Keyframe: -1, TimeIndex: -1,
			Err: fmt.Errorf("scale numerator and denominator must be in [1,64]")}
	}
	if c.Grid < 1 || c.Grid > 8 {
		return &Error{Code: CodeParam, Op: "validate", Field: "grid", Keyframe: -1, TimeIndex: -1,
			Err: fmt.Errorf("must be in [1,8]")}
	}
	if c.FillRule != FillNonZero && c.FillRule != FillEvenOdd {
		return &Error{Code: CodeParam, Op: "validate", Field: "fillRule", Keyframe: -1, TimeIndex: -1,
			Err: fmt.Errorf("unsupported fill rule %q", c.FillRule)}
	}
	if c.Margin < 0 || c.Margin > 64 {
		return &Error{Code: CodeParam, Op: "validate", Field: "margin", Keyframe: -1, TimeIndex: -1,
			Err: fmt.Errorf("must be in [0,64]")}
	}
	if c.MaxWorkers < 1 || c.MaxWorkers > MaxWorkersHardLimit {
		return &Error{Code: CodeParam, Op: "validate", Field: "maxWorkers", Keyframe: -1, TimeIndex: -1,
			Err: fmt.Errorf("must be in [1,%d]", MaxWorkersHardLimit)}
	}
	if c.MaxPixelBufferBytes < 1<<20 || c.MaxPixelBufferBytes > MaxBufferBytesHardLimit {
		return &Error{Code: CodeParam, Op: "validate", Field: "maxPixelBufferBytes", Keyframe: -1, TimeIndex: -1,
			Err: fmt.Errorf("must be in [%d,%d]", int64(1<<20), MaxBufferBytesHardLimit)}
	}
	return nil
}

func (s *Scene) Clone() *Scene {
	if s == nil {
		return nil
	}
	cp := *s
	cp.SampleTimes = append([]int64(nil), s.SampleTimes...)
	cp.Paths = make([]Path, len(s.Paths))
	for i, p := range s.Paths {
		cp.Paths[i] = p.Clone()
	}
	return &cp
}

func (p Path) Clone() Path {
	cp := p
	cp.Keyframes = make([]Keyframe, len(p.Keyframes))
	for i, k := range p.Keyframes {
		cp.Keyframes[i] = k.Clone()
	}
	return cp
}

func (k Keyframe) Clone() Keyframe {
	cp := k
	cp.Contour.Segments = append([]Segment(nil), k.Contour.Segments...)
	return cp
}

func (c Contour) Clone() Contour {
	cp := c
	cp.Segments = append([]Segment(nil), c.Segments...)
	return cp
}

func (p Point) Add(q Point) (Point, error) {
	x, ok := checkedAdd(p.X, q.X)
	if !ok {
		return Point{}, errorf(CodeOverflow, "point.add", "x coordinate overflow")
	}
	y, ok := checkedAdd(p.Y, q.Y)
	if !ok {
		return Point{}, errorf(CodeOverflow, "point.add", "y coordinate overflow")
	}
	return Point{X: x, Y: y}, nil
}

func (p Point) Sub(q Point) (Point, error) {
	x, ok := checkedSub(p.X, q.X)
	if !ok {
		return Point{}, errorf(CodeOverflow, "point.sub", "x coordinate overflow")
	}
	y, ok := checkedSub(p.Y, q.Y)
	if !ok {
		return Point{}, errorf(CodeOverflow, "point.sub", "y coordinate overflow")
	}
	return Point{X: x, Y: y}, nil
}

type Bounds struct {
	MinX int64 `json:"minX"`
	MinY int64 `json:"minY"`
	MaxX int64 `json:"maxX"`
	MaxY int64 `json:"maxY"`
}

func EmptyBounds() Bounds {
	return Bounds{MinX: int64(^uint64(0) >> 1), MinY: int64(^uint64(0) >> 1),
		MaxX: -int64(^uint64(0)>>1) - 1, MaxY: -int64(^uint64(0)>>1) - 1}
}

func (b *Bounds) AddPoint(p Point) {
	if p.X < b.MinX {
		b.MinX = p.X
	}
	if p.Y < b.MinY {
		b.MinY = p.Y
	}
	if p.X > b.MaxX {
		b.MaxX = p.X
	}
	if p.Y > b.MaxY {
		b.MaxY = p.Y
	}
}

func (b *Bounds) Union(other Bounds) {
	if other.Empty() {
		return
	}
	b.AddPoint(Point{X: other.MinX, Y: other.MinY})
	b.AddPoint(Point{X: other.MaxX, Y: other.MaxY})
}

func (b Bounds) Empty() bool { return b.MinX > b.MaxX || b.MinY > b.MaxY }

func (b Bounds) Width() int64  { return b.MaxX - b.MinX }
func (b Bounds) Height() int64 { return b.MaxY - b.MinY }

func (b Bounds) Expand(by int64) Bounds {
	if b.Empty() {
		return b
	}
	return Bounds{MinX: b.MinX - by, MinY: b.MinY - by, MaxX: b.MaxX + by, MaxY: b.MaxY + by}
}

func HashValue(v any) string {
	data, err := json.Marshal(v)
	if err != nil {
		panic(fmt.Sprintf("hash marshal: %v", err))
	}
	sum := sha256.Sum256(data)
	return hex.EncodeToString(sum[:])
}

func (p Path) ContentHash() string {
	return HashValue(struct {
		ID        string
		Paint     Paint
		Keyframes []Keyframe
	}{p.ID, p.Paint, p.Keyframes})
}

func (p Paint) Validate(pathID string) error {
	if p.Opacity < 0 || p.Opacity > 10000 {
		return &Error{Code: CodeMode, Op: "validate", PathID: pathID, Field: "opacity", Keyframe: -1, TimeIndex: -1,
			Err: fmt.Errorf("opacity %d outside [0,10000]", p.Opacity)}
	}
	switch p.Blend {
	case BlendSourceOver, BlendNormal, BlendAdd, BlendMultiply, BlendScreen:
	default:
		return &Error{Code: CodeMode, Op: "validate", PathID: pathID, Field: "blendMode", Keyframe: -1, TimeIndex: -1,
			Err: fmt.Errorf("unsupported blend mode %q", p.Blend)}
	}
	if p.StrokeWidth < 0 {
		return &Error{Code: CodeParam, Op: "validate", PathID: pathID, Field: "strokeWidth", Keyframe: -1, TimeIndex: -1,
			Err: fmt.Errorf("stroke width must be non-negative")}
	}
	if p.StrokeWidth > 0 {
		switch p.Join {
		case JoinRound, JoinBevel, JoinMiter:
		default:
			return &Error{Code: CodeParam, Op: "validate", PathID: pathID, Field: "strokeJoin", Keyframe: -1, TimeIndex: -1,
				Err: fmt.Errorf("unsupported join %q", p.Join)}
		}
	}
	if !p.Fill && p.StrokeWidth == 0 {
		return &Error{Code: CodeSchema, Op: "validate", PathID: pathID, Field: "paint", Keyframe: -1, TimeIndex: -1,
			Err: fmt.Errorf("path has neither fill nor stroke")}
	}
	return nil
}

func (s *Scene) Validate() error {
	if s == nil {
		return newError(CodeSchema, "validate.scene", fmt.Errorf("nil scene"))
	}
	if s.ID == "" {
		return &Error{Code: CodeSchema, Op: "validate", Field: "sceneId", SourceVersion: s.SourceVersion, Keyframe: -1, TimeIndex: -1}
	}
	if s.FPS <= 0 || s.FPS > 10_000 {
		return &Error{Code: CodeSchema, Op: "validate", Field: "fps", SourceVersion: s.SourceVersion, Keyframe: -1, TimeIndex: -1,
			Err: fmt.Errorf("must be in [1,10000]")}
	}
	if len(s.SampleTimes) == 0 {
		return &Error{Code: CodeSchema, Op: "validate", Field: "sampleTimes", SourceVersion: s.SourceVersion, Keyframe: -1, TimeIndex: -1}
	}
	if len(s.Paths) == 0 {
		return &Error{Code: CodeSchema, Op: "validate", Field: "paths", SourceVersion: s.SourceVersion, Keyframe: -1, TimeIndex: -1}
	}
	seenIDs := make(map[string]bool, len(s.Paths))
	for pi := range s.Paths {
		p := &s.Paths[pi]
		if p.ID == "" {
			return &Error{Code: CodeSchema, Op: "validate", Field: fmt.Sprintf("paths[%d].id", pi), SourceVersion: s.SourceVersion, Keyframe: -1, TimeIndex: -1}
		}
		if seenIDs[p.ID] {
			return &Error{Code: CodeSchema, Op: "validate", PathID: p.ID, Field: fmt.Sprintf("paths[%d].id", pi), SourceVersion: s.SourceVersion, Keyframe: -1, TimeIndex: -1,
				Err: fmt.Errorf("duplicate path id")}
		}
		seenIDs[p.ID] = true
		if err := p.Paint.Validate(p.ID); err != nil {
			if e, ok := err.(*Error); ok {
				e.SourceVersion = s.SourceVersion
			}
			return err
		}
		if len(p.Keyframes) == 0 {
			return &Error{Code: CodeSchema, Op: "validate", PathID: p.ID, Field: fmt.Sprintf("paths[%d].keyframes", pi), SourceVersion: s.SourceVersion, Keyframe: -1, TimeIndex: -1}
		}
		for ki := range p.Keyframes {
			k := &p.Keyframes[ki]
			if len(k.Contour.Segments) == 0 {
				return &Error{Code: CodeSchema, Op: "validate", PathID: p.ID, Keyframe: ki,
					Field: fmt.Sprintf("paths[%d].keyframes[%d].contour.segments", pi, ki), SourceVersion: s.SourceVersion, TimeIndex: -1}
			}
			if ki > 0 && p.Keyframes[ki-1].Time >= k.Time {
				return &Error{Code: CodeSchema, Op: "validate", PathID: p.ID, Keyframe: ki,
					Field: fmt.Sprintf("paths[%d].keyframes[%d].time", pi, ki), SourceVersion: s.SourceVersion, TimeIndex: -1,
					Err: fmt.Errorf("keyframe times must be strictly increasing")}
			}
			for si := range k.Contour.Segments {
				seg := &k.Contour.Segments[si]
				if seg.Kind != SegmentQuad && seg.Kind != SegmentCubic {
					return &Error{Code: CodeSchema, Op: "validate", PathID: p.ID, Keyframe: ki, SourceVersion: s.SourceVersion,
						Field: fmt.Sprintf("paths[%d].keyframes[%d].contour.segments[%d].kind", pi, ki, si), TimeIndex: -1}
				}
			}
		}
		for ki := 1; ki < len(p.Keyframes); ki++ {
			if len(p.Keyframes[ki].Contour.Segments) != len(p.Keyframes[0].Contour.Segments) {
				return &Error{Code: CodeSchema, Op: "validate", PathID: p.ID, Keyframe: ki, SourceVersion: s.SourceVersion,
					Field: fmt.Sprintf("paths[%d].keyframes[%d].contour.segments", pi, ki), TimeIndex: -1,
					Err: fmt.Errorf("all keyframes must share segment topology")}
			}
			for si := range p.Keyframes[ki].Contour.Segments {
				if p.Keyframes[ki].Contour.Segments[si].Kind != p.Keyframes[0].Contour.Segments[si].Kind {
					return &Error{Code: CodeSchema, Op: "validate", PathID: p.ID, Keyframe: ki, SourceVersion: s.SourceVersion,
						Field: fmt.Sprintf("paths[%d].keyframes[%d].contour.segments[%d].kind", pi, ki, si), TimeIndex: -1,
						Err: fmt.Errorf("segment topology differs")}
				}
			}
		}
	}
	if !sort.SliceIsSorted(s.SampleTimes, func(i, j int) bool { return s.SampleTimes[i] < s.SampleTimes[j] }) {
		return &Error{Code: CodeSchema, Op: "validate", Field: "sampleTimes", SourceVersion: s.SourceVersion, Keyframe: -1, TimeIndex: -1,
			Err: fmt.Errorf("sample times must be strictly increasing")}
	}
	for i := 1; i < len(s.SampleTimes); i++ {
		if s.SampleTimes[i] == s.SampleTimes[i-1] {
			return &Error{Code: CodeSchema, Op: "validate", Field: "sampleTimes", SourceVersion: s.SourceVersion, Keyframe: -1, TimeIndex: i,
				Err: fmt.Errorf("duplicate sample time")}
		}
	}
	return nil
}

func ParseHexColor(value string) (RGBA, error) {
	v := strings.TrimPrefix(strings.TrimSpace(value), "#")
	if len(v) == 6 {
		v += "ff"
	}
	if len(v) != 8 {
		return RGBA{}, fmt.Errorf("expected #RRGGBBAA")
	}
	n, err := strconv.ParseUint(v, 16, 32)
	if err != nil {
		return RGBA{}, err
	}
	return RGBA{R: uint8(n >> 24), G: uint8(n >> 16), B: uint8(n >> 8), A: uint8(n)}, nil
}

func (c RGBA) Hex() string {
	return fmt.Sprintf("#%02x%02x%02x%02x", c.R, c.G, c.B, c.A)
}
