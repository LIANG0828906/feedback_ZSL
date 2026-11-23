package morph

import (
	"math"
	"sort"
)

type FlattenedPath struct {
	PathID             string    `json:"path_id"`
	Time               float64   `json:"time"`
	Points             []Vec2    `json:"points"`
	GeometryBBox       BBox      `json:"geometry_bbox"`
	StrokeBBox         BBox      `json:"stroke_bbox"`
	SegmentCount       int       `json:"segment_count"`
	InterpolationCount int       `json:"interpolation_count"`
	ContourArea        float64   `json:"contour_area"`
	PolylineLength     float64   `json:"polyline_length"`
	Segments           []Segment `json:"-"`
}

func ValidateEvalConfig(cfg EvalConfig, scene *Scene) error {
	if !finite(cfg.Scale) || cfg.Scale <= 0 {
		return withField(newError(CodeParam, "scale must be finite and positive"), "scale")
	}
	if !finite(cfg.MidTolerance) || cfg.MidTolerance <= 0 {
		return withField(newError(CodeParam, "midpoint flattening tolerance must be positive"), "mid_tolerance")
	}
	if !allowedInterpolation[cfg.Interpolation] {
		return withField(newError(CodeParam, "unsupported interpolation"), "interpolation")
	}
	return nil
}

func ValidateRenderConfig(cfg RenderConfig) error {
	if cfg.Grid < 1 || cfg.Grid > 8 {
		return withField(newError(CodeParam, "subsample grid must be in [1,8]"), "grid")
	}
	if cfg.PaddingPixels < 0 {
		return withField(newError(CodeParam, "padding must be non-negative"), "padding_pixels")
	}
	if cfg.MaxFramePixels <= 0 || cfg.MaxFramePixels > 8192 {
		return withField(newError(CodeParam, "max frame pixels must be in [1,8192]"), "max_frame_pixels")
	}
	return nil
}

// InterpolatePath evaluates all control points at t. The returned segments are
// a deep copy and preserve the topology of the surrounding keyframes.
func InterpolatePath(p *Path, t float64, method string) ([]Segment, int, error) {
	if !allowedInterpolation[method] {
		return nil, 0, withPath(withField(newError(CodeParam, "unsupported interpolation"), "interpolation"), p.ID)
	}
	if len(p.Keyframes) == 0 {
		return nil, 0, withPath(withField(newError(CodeKey, "path has no keyframes"), "keyframes"), p.ID)
	}
	if t < p.Keyframes[0].Time || t > p.Keyframes[len(p.Keyframes)-1].Time {
		e := withPath(withField(newError(CodeParam, "sample time is outside keyframe range"), "sample_time"), p.ID)
		e.Time = t
		return nil, 0, e
	}
	if t == p.Keyframes[len(p.Keyframes)-1].Time {
		return cloneSegments(p.Keyframes[len(p.Keyframes)-1].Segments), 0, nil
	}
	idx := sort.Search(len(p.Keyframes), func(i int) bool { return p.Keyframes[i].Time > t }) - 1
	if idx < 0 {
		idx = 0
	}
	if idx >= len(p.Keyframes)-1 {
		idx = len(p.Keyframes) - 2
	}
	a := p.Keyframes[idx]
	b := p.Keyframes[idx+1]
	u := (t - a.Time) / (b.Time - a.Time)
	u = applyEasing(u, method)
	if len(a.Segments) != len(b.Segments) {
		return nil, 0, withPath(withField(newError(CodeKey, "keyframe topology mismatch"), "segments"), p.ID)
	}
	out := make([]Segment, len(a.Segments))
	interpolations := 0
	for i := range a.Segments {
		sa, sb := a.Segments[i], b.Segments[i]
		if sa.Kind != sb.Kind {
			return nil, 0, withPath(withField(newError(CodeKey, "keyframe segment kind mismatch"), "segments"), p.ID)
		}
		seg := Segment{Kind: sa.Kind}
		seg.P0 = lerpVec(sa.P0, sb.P0, u)
		seg.P1 = lerpVec(sa.P1, sb.P1, u)
		seg.P2 = lerpVec(sa.P2, sb.P2, u)
		if sa.Kind == "C" {
			if sa.P3 == nil || sb.P3 == nil {
				return nil, 0, withPath(withField(newError(CodePath, "cubic segment misses p3"), "segments"), p.ID)
			}
			seg.P3 = VecPtr(lerpVec(*sa.P3, *sb.P3, u))
		}
		out[i] = seg
		interpolations += len(seg.Points()) * 2
	}
	return out, interpolations, nil
}

func applyEasing(u float64, method string) float64 {
	u = math.Max(0, math.Min(1, u))
	switch method {
	case "smoothstep":
		return u * u * (3 - 2*u)
	case "smootherstep":
		return u * u * u * (u*(u*6-15) + 10)
	default:
		return u
	}
}

func lerpVec(a, b Vec2, u float64) Vec2 {
	return Vec2{X: a.X + (b.X-a.X)*u, Y: a.Y + (b.Y-a.Y)*u}
}

func cloneSegments(in []Segment) []Segment {
	out := make([]Segment, len(in))
	for i, s := range in {
		out[i] = s
		if s.P3 != nil {
			out[i].P3 = VecPtr(*s.P3)
		}
	}
	return out
}

// FlattenSegments performs adaptive midpoint subdivision. The subdivision tree
// is fixed; lowering tolerance can only refine leaves, so segment counts are
// monotone non-decreasing.
func FlattenSegments(segments []Segment, tolerance float64) ([]Vec2, int) {
	if len(segments) == 0 {
		return nil, 0
	}
	pts := []Vec2{segments[0].P0}
	count := 0
	var walk func(Segment, int)
	walk = func(seg Segment, depth int) {
		if depth >= 24 || segmentFlatness(seg) <= tolerance {
			end := seg.End()
			if len(pts) == 0 || pts[len(pts)-1].Distance(end) > 1e-12 {
				pts = append(pts, end)
				count++
			}
			return
		}
		left, right := splitSegment(seg)
		walk(left, depth+1)
		walk(right, depth+1)
	}
	for _, s := range segments {
		walk(s, 0)
	}
	if len(pts) > 1 {
		pts[len(pts)-1] = pts[0]
	}
	return pts, count
}

func segmentFlatness(s Segment) float64 {
	end := s.End()
	line := end.Sub(s.P0)
	if line.Len() < 1e-14 {
		return math.Max(s.P0.Distance(s.P1), s.P2.Distance(s.P0))
	}
	d1 := pointLineDistance(s.P1, s.P0, end)
	if s.Kind == "C" && s.P3 != nil {
		return math.Max(d1, pointLineDistance(s.P2, s.P0, end))
	}
	return d1
}

func pointLineDistance(p, a, b Vec2) float64 {
	ab := b.Sub(a)
	l := ab.Len()
	if l < 1e-14 {
		return p.Distance(a)
	}
	return math.Abs(ab.Cross(p.Sub(a))) / l
}

func splitSegment(s Segment) (Segment, Segment) {
	if s.Kind == "C" && s.P3 != nil {
		p01 := lerpVec(s.P0, s.P1, 0.5)
		p12 := lerpVec(s.P1, s.P2, 0.5)
		p23 := lerpVec(s.P2, *s.P3, 0.5)
		p012 := lerpVec(p01, p12, 0.5)
		p123 := lerpVec(p12, p23, 0.5)
		mid := lerpVec(p012, p123, 0.5)
		return Segment{Kind: "C", P0: s.P0, P1: p01, P2: p012, P3: VecPtr(mid)},
			Segment{Kind: "C", P0: mid, P1: p123, P2: p23, P3: VecPtr(*s.P3)}
	}
	p01 := lerpVec(s.P0, s.P1, 0.5)
	p12 := lerpVec(s.P1, s.P2, 0.5)
	mid := lerpVec(p01, p12, 0.5)
	return Segment{Kind: "Q", P0: s.P0, P1: p01, P2: mid},
		Segment{Kind: "Q", P0: mid, P1: p12, P2: s.P2}
}

func FlattenPath(p *Path, t float64, cfg EvalConfig) (*FlattenedPath, error) {
	if err := ValidateEvalConfig(cfg, nil); err != nil {
		return nil, err
	}
	segs, interp, err := InterpolatePath(p, t, cfg.Interpolation)
	if err != nil {
		return nil, err
	}
	pts, count := FlattenSegments(segs, cfg.MidTolerance)
	b := SegmentsBBox(segs)
	sb := b
	radius := 0.0
	if p.Stroke != nil {
		radius = p.Stroke.Width * 0.5
	}
	if radius > 0 {
		sb = b.Expand(radius)
	}
	return &FlattenedPath{
		PathID: p.ID, Time: t, Points: pts, GeometryBBox: b, StrokeBBox: sb,
		SegmentCount: count, InterpolationCount: interp,
		ContourArea: ContourArea(segs), PolylineLength: PolylineLength(pts),
		Segments: segs,
	}, nil
}
