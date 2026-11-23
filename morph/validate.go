package morph

import (
	"fmt"
	"math"
)

var allowedBlendModes = map[string]bool{
	"over": true, "add": true, "multiply": true, "screen": true, "replace": true,
}

var allowedJoins = map[string]bool{"round": true, "bevel": true, "miter": true}
var allowedFillRules = map[string]bool{"nonzero": true, "evenodd": true}
var allowedInterpolation = map[string]bool{"linear": true, "smoothstep": true, "smootherstep": true}

func ValidateScene(s *Scene) error {
	if s == nil {
		return newError(CodeDecode, "scene is nil")
	}
	if s.Format != SceneFormat {
		e := newError(CodeDecode, "unsupported or missing scene format")
		e.Field = "format"
		return e
	}
	if s.Version != 1 {
		e := newError(CodeDecode, "unsupported scene version")
		e.Field = "version"
		return e
	}
	if s.ID == "" {
		return withField(newError(CodeDecode, "scene id is required"), "id")
	}
	if !finite(s.Quantum) || s.Quantum <= 0 {
		return withField(newError(CodeStyle, "quantum must be finite and positive"), "quantum")
	}
	if !finite(s.CoordMin) || !finite(s.CoordMax) || s.CoordMin >= s.CoordMax {
		return withField(newError(CodeStyle, "coordinate range is invalid"), "coord_min")
	}
	if !finite(s.Render.Scale) || s.Render.Scale <= 0 {
		return withField(newError(CodeStyle, "render scale must be positive"), "render_defaults.scale")
	}
	if s.Render.PaddingPixels < 0 {
		return withField(newError(CodeStyle, "padding must be non-negative"), "render_defaults.padding_pixels")
	}
	if s.Render.MaxFramePixels <= 0 {
		return withField(newError(CodeStyle, "max frame pixels must be positive"), "render_defaults.max_frame_pixels")
	}
	if len(s.Paths) == 0 {
		return withField(newError(CodePath, "scene must contain at least one path"), "paths")
	}
	if len(s.SampleTimes) == 0 {
		return withField(newError(CodeKey, "scene must contain sample times"), "sample_times")
	}
	for i := 1; i < len(s.SampleTimes); i++ {
		if !finite(s.SampleTimes[i]) || s.SampleTimes[i] <= s.SampleTimes[i-1] {
			e := withField(newError(CodeKey, "sample times must be finite and strictly increasing"), "sample_times")
			e.Sample = i
			e.Time = s.SampleTimes[i]
			return e
		}
	}
	seen := make(map[string]bool, len(s.Paths))
	for pi := range s.Paths {
		p := &s.Paths[pi]
		if p.ID == "" {
			e := withField(newError(CodePath, "path id is required"), "id")
			e.Sample = -1
			return e
		}
		if seen[p.ID] {
			e := withPath(newError(CodePath, "duplicate path id"), p.ID)
			e.Field = "id"
			return e
		}
		seen[p.ID] = true
		if err := validatePath(p, s); err != nil {
			return err
		}
	}
	return nil
}

func validatePath(p *Path, s *Scene) error {
	if len(p.Keyframes) < 2 {
		return withPath(withField(newError(CodeKey, "at least two keyframes are required"), "keyframes"), p.ID)
	}
	if !finite(p.Opacity) || p.Opacity < 0 || p.Opacity > 1 {
		return withPath(withField(newError(CodeStyle, "opacity must be in [0,1]"), "opacity"), p.ID)
	}
	if !allowedBlendModes[p.BlendMode] {
		return withPath(withField(newError(CodeStyle, "unsupported blend mode"), "blend_mode"), p.ID)
	}
	if p.FillRule == "" {
		p.FillRule = "nonzero"
	}
	if !allowedFillRules[p.FillRule] {
		return withPath(withField(newError(CodeStyle, "unsupported fill rule"), "fill_rule"), p.ID)
	}
	if p.Fill == nil && p.Stroke == nil {
		return withPath(withField(newError(CodeStyle, "path needs a fill or stroke style"), "style"), p.ID)
	}
	if p.Fill != nil {
		if err := validateColor(p.Fill.Color, p.ID, "fill.color"); err != nil {
			return err
		}
	}
	if p.Stroke != nil {
		if err := validateColor(p.Stroke.Color, p.ID, "stroke.color"); err != nil {
			return err
		}
		if !finite(p.Stroke.Width) || p.Stroke.Width <= 0 {
			return withPath(withField(newError(CodeStyle, "stroke width must be positive"), "stroke.width"), p.ID)
		}
		if !allowedJoins[p.Stroke.Join] {
			return withPath(withField(newError(CodeStyle, "unsupported stroke join"), "stroke.join"), p.ID)
		}
		if p.Stroke.MiterLimit == 0 {
			p.Stroke.MiterLimit = 4
		}
		if !finite(p.Stroke.MiterLimit) || p.Stroke.MiterLimit < 1 {
			return withPath(withField(newError(CodeStyle, "miter limit must be at least 1"), "stroke.miter_limit"), p.ID)
		}
	}
	prevT := math.Inf(-1)
	for ki := range p.Keyframes {
		k := &p.Keyframes[ki]
		if !finite(k.Time) || k.Time <= prevT {
			e := withPath(withField(newError(CodeKey, "keyframe times must be finite and strictly increasing"), "keyframes.time"), p.ID)
			e.Sample = ki
			e.Time = k.Time
			return e
		}
		prevT = k.Time
		if len(k.Segments) == 0 {
			return withPath(withField(newError(CodePath, "keyframe has no segments"), "keyframes.segments"), p.ID)
		}
		for si := range k.Segments {
			seg := &k.Segments[si]
			if seg.Kind != "Q" && seg.Kind != "C" {
				return withPath(withField(newError(CodePath, "segment kind must be Q or C"), fmt.Sprintf("keyframes[%d].segments[%d].kind", ki, si)), p.ID)
			}
			if seg.Kind == "C" && seg.P3 == nil {
				return withPath(withField(newError(CodePath, "cubic segment misses p3"), fmt.Sprintf("keyframes[%d].segments[%d].p3", ki, si)), p.ID)
			}
			for _, pt := range seg.Points() {
				if err := validatePoint(pt, s, p.ID, fmt.Sprintf("keyframes[%d].segments[%d]", ki, si)); err != nil {
					return err
				}
			}
			if si > 0 {
				prev := k.Segments[si-1]
				if prev.End().Distance(seg.P0) > s.Quantum*0.51 {
					return withPath(withField(newError(CodePath, "curve segments are not continuous"), fmt.Sprintf("keyframes[%d].segments[%d].p0", ki, si)), p.ID)
				}
			}
		}
		first := k.Segments[0].P0
		last := k.Segments[len(k.Segments)-1].End()
		if first.Distance(last) > s.Quantum*0.51 {
			return withPath(withField(newError(CodePath, "contour is not closed"), fmt.Sprintf("keyframes[%d].segments[%d].end", ki, len(k.Segments)-1)), p.ID)
		}
		if ki > 0 {
			prevK := p.Keyframes[ki-1]
			if len(prevK.Segments) != len(k.Segments) {
				return withPath(withField(newError(CodeKey, "keyframes must preserve segment topology"), fmt.Sprintf("keyframes[%d].segments", ki)), p.ID)
			}
			for si := range k.Segments {
				if prevK.Segments[si].Kind != k.Segments[si].Kind {
					return withPath(withField(newError(CodeKey, "keyframes must preserve segment kinds"), fmt.Sprintf("keyframes[%d].segments[%d].kind", ki, si)), p.ID)
				}
			}
		}
	}
	for i, t := range s.SampleTimes {
		if t < p.Keyframes[0].Time || t > p.Keyframes[len(p.Keyframes)-1].Time {
			e := withPath(withField(newError(CodeParam, "sample time is outside keyframe range"), "sample_times"), p.ID)
			e.Sample = i
			e.Time = t
			return e
		}
	}
	return nil
}

func validatePoint(p Vec2, s *Scene, pathID, field string) error {
	if !finite(p.X) || !finite(p.Y) {
		return withPath(withField(newError(CodeStyle, "control point must be finite"), field), pathID)
	}
	if p.X < s.CoordMin || p.X > s.CoordMax || p.Y < s.CoordMin || p.Y > s.CoordMax {
		return withPath(withField(newError(CodeStyle, "control point is outside the quantization range"), field), pathID)
	}
	if math.Abs(p.X/s.Quantum-math.Round(p.X/s.Quantum)) > 0.01 ||
		math.Abs(p.Y/s.Quantum-math.Round(p.Y/s.Quantum)) > 0.01 {
		return withPath(withField(newError(CodeStyle, "control point is not on the quantization grid"), field), pathID)
	}
	return nil
}

func validateColor(c [3]float64, pathID, field string) error {
	for _, v := range c {
		if !finite(v) || v < 0 || v > 1 {
			return withPath(withField(newError(CodeStyle, "color channel must be in [0,1]"), field), pathID)
		}
	}
	return nil
}

func ValidateTruth(s *Scene, t *Truth) error {
	if t == nil {
		return withField(newError(CodeDecode, "truth manifest is missing"), "truth")
	}
	if t.Format != TruthFormat {
		return withField(newError(CodeDecode, "unsupported truth format"), "truth.format")
	}
	if t.SceneID != s.ID || t.Seed != s.Seed {
		return withField(newError(CodeDecode, "truth does not belong to scene"), "truth.scene_id")
	}
	if len(t.Samples) != len(s.SampleTimes) {
		return withField(newError(CodeDecode, "truth sample count mismatch"), "truth.samples")
	}
	for i := range t.Samples {
		ts := &t.Samples[i]
		if ts.Index != i || math.Abs(ts.Time-s.SampleTimes[i]) > 1e-12 {
			e := withField(newError(CodeDecode, "truth sample metadata mismatch"), "truth.samples")
			e.Sample = i
			return e
		}
		if ts.Width <= 0 || ts.Height <= 0 {
			e := withField(newError(CodeDecode, "truth frame size is invalid"), "truth.samples")
			e.Sample = i
			return e
		}
		if len(ts.Paths) != len(s.Paths) {
			e := withField(newError(CodeDecode, "truth path count mismatch"), "truth.samples")
			e.Sample = i
			return e
		}
		for pi := range ts.Paths {
			pt := &ts.Paths[pi]
			if pt.PathID != s.Paths[pi].ID {
				e := withPath(withField(newError(CodeDecode, "truth path id mismatch"), "truth.paths.path_id"), pt.PathID)
				e.Sample = i
				return e
			}
			if !pt.Closed {
				e := withPath(withField(newError(CodePath, "truth reports an open contour"), "truth.paths.closed"), pt.PathID)
				e.Sample = i
				return e
			}
			if pt.GeometryBBox.IsEmpty() || !finite(pt.ContourArea) || !finite(pt.ReferenceCoverageArea) {
				e := withPath(withField(newError(CodeDecode, "truth path metrics are invalid"), "truth.paths"), pt.PathID)
				e.Sample = i
				return e
			}
		}
	}
	return nil
}

func finite(v float64) bool { return !math.IsNaN(v) && !math.IsInf(v, 0) }
