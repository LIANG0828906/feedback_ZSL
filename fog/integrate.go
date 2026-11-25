package fog

import (
	"fmt"
	"hash/fnv"
	"math"
	"sort"
)

func PhaseFunction(kind PhaseKind, g, cosTheta float64) float64 {
	cosTheta = clamp(cosTheta, -1, 1)
	switch kind {
	case PhaseIsotropic, "":
		return 1.0 / (4.0 * math.Pi)
	case PhaseHG:
		g = clamp(g, -0.999, 0.999)
		den := 1 + g*g - 2*g*cosTheta
		return (1 - g*g) / (4 * math.Pi * math.Pow(math.Max(den, 1e-9), 1.5))
	default:
		return 1.0 / (4.0 * math.Pi)
	}
}

type ShadowFunc func(ray Ray, p Vec3, stepIndex, sampleIndex int, activeIDs []string) float64

// DeterministicShadow performs jittered directional shadow rays against
// non-active media bounds. It is stable under worker scheduling.
func DeterministicShadow(media []*Medium, light Light) ShadowFunc {
	byID := make(map[string]*Medium, len(media))
	for _, m := range media {
		byID[m.ID] = m
	}
	return func(ray Ray, p Vec3, stepIndex, sampleIndex int, activeIDs []string) float64 {
		if light.ShadowJitter <= 0 {
			return shadowBinary(media, activeIDs, p, light.Direction)
		}
		h := fnv.New64a()
		_, _ = h.Write([]byte(fmt.Sprintf("%s|%d|%d|%.6f|%.6f|%.6f", ray.ID, stepIndex, sampleIndex, p.X, p.Y, p.Z)))
		sum := h.Sum64()
		a := deterministicUnit((sum & 0xffff) >> 0)
		b := deterministicUnit((sum >> 16) & 0xffff)
		c := deterministicUnit((sum >> 32) & 0xffff)
		_ = c
		l := light.Direction.Normalize()
		tmp := Vec3{X: 0, Y: 1, Z: 0}
		if math.Abs(l.Dot(tmp)) > 0.9 {
			tmp = Vec3{X: 1, Y: 0, Z: 0}
		}
		right := l.Cross(tmp).Normalize()
		up := right.Cross(l).Normalize()
		dir := l.Add(right.Mul(a * light.ShadowJitter)).Add(up.Mul(b * light.ShadowJitter)).Normalize()
		return shadowBinary(media, activeIDs, p, dir)
	}
}

func (v Vec3) Cross(o Vec3) Vec3 {
	return Vec3{
		X: v.Y*o.Z - v.Z*o.Y,
		Y: v.Z*o.X - v.X*o.Z,
		Z: v.X*o.Y - v.Y*o.X,
	}
}

func deterministicUnit(v uint64) float64 {
	return (float64(v%10000)/9999.0)*2 - 1
}

func shadowBinary(media []*Medium, activeIDs []string, p, dir Vec3) float64 {
	active := map[string]bool{}
	for _, id := range activeIDs {
		active[id] = true
	}
	origin := p.Add(dir.Mul(1e-4))
	for _, m := range media {
		if m == nil || !m.CastShadows || active[m.ID] {
			continue
		}
		if _, _, ok := m.Bounds.IntersectRay(origin, dir); ok {
			return 0
		}
	}
	return 1
}

type SegmentResult struct {
	Index              int      `json:"index"`
	Kind               SpanKind `json:"kind"`
	MediumIDs          []string `json:"medium_ids,omitempty"`
	TMin               float64  `json:"t_min"`
	TMax               float64  `json:"t_max"`
	Transmittance      float64  `json:"transmittance"`
	CumulativeT        float64  `json:"cumulative_t"`
	Radiance           float64  `json:"radiance"`
	CumulativeRadiance float64  `json:"cumulative_radiance"`
	Steps              int      `json:"steps"`
	ShadowSamples      int      `json:"shadow_samples"`
}

type RayResult struct {
	Trace             TraceResult     `json:"trace"`
	Transmittance     float64         `json:"transmittance"`
	Radiance          float64         `json:"radiance"`
	Segments          []SegmentResult `json:"segments"`
	StepsUsed         int             `json:"steps_used"`
	ShadowSamplesUsed int             `json:"shadow_samples_used"`
}

type Integrator struct {
	Media         []*Medium
	Recipe        Recipe
	Light         Light
	Shadow        ShadowFunc
	Steps         int
	ShadowSamples int
}

func NewIntegrator(s *Scenario, mode RenderMode) Integrator {
	steps := s.Recipe.Steps
	shadows := s.Recipe.ShadowSamples
	if mode == ModePreview {
		steps = s.Recipe.PreviewSteps
		shadows = s.Recipe.PreviewShadowSamples
	}
	return Integrator{
		Media: s.Media, Recipe: s.Recipe, Light: s.Light,
		Shadow: DeterministicShadow(s.Media, s.Light),
		Steps:  steps, ShadowSamples: shadows,
	}
}

// ShadowCost returns the deterministic number of shadow samples required.
func (in Integrator) ShadowCost(tr TraceResult) int64 {
	if in.ShadowSamples <= 0 {
		return 0
	}
	var steps int64
	for _, span := range tr.Spans {
		if span.Kind == SpanMedium {
			steps += int64(in.Steps)
		}
	}
	return steps * int64(in.ShadowSamples)
}

func (in Integrator) Integrate(tr TraceResult) (RayResult, error) {
	byID := make(map[string]*Medium, len(in.Media))
	for _, m := range in.Media {
		byID[m.ID] = m
	}
	out := RayResult{Trace: tr, Transmittance: 1}
	cumT := 1.0
	cumL := 0.0

	for _, span := range tr.Spans {
		seg := SegmentResult{
			Index: span.Index, Kind: span.Kind, MediumIDs: append([]string(nil), span.MediumIDs...),
			TMin: span.TMin, TMax: span.TMax, Transmittance: 1, CumulativeT: cumT,
		}
		if span.Kind == SpanEmpty || len(span.MediumIDs) == 0 {
			seg.CumulativeT = cumT
			seg.CumulativeRadiance = cumL
			out.Segments = append(out.Segments, seg)
			continue
		}

		length := span.TMax - span.TMin
		dt := length / float64(in.Steps)
		seg.Steps = in.Steps
		seg.ShadowSamples = in.Steps * in.ShadowSamples
		out.StepsUsed += in.Steps
		out.ShadowSamplesUsed += seg.ShadowSamples

		for step := 0; step < in.Steps; step++ {
			t := span.TMin + (float64(step)+0.5)*dt
			p := tr.Ray.Point(t)
			sigmaT, source := in.materialAt(tr.Ray, p, step, span.MediumIDs)
			optical := sigmaT * dt
			stepT := math.Exp(-optical)
			if sigmaT > 1e-15 {
				cumL += cumT * (source / sigmaT) * (-math.Expm1(-optical))
				seg.Radiance += cumT * (source / sigmaT) * (-math.Expm1(-optical))
			}
			cumT *= stepT
			seg.Transmittance *= stepT
		}
		seg.CumulativeT = cumT
		seg.CumulativeRadiance = cumL
		if err := checkTransmittance(cumT, tr.RayID, fmt.Sprintf("span-%d", span.Index)); err != nil {
			return out, err
		}
		out.Segments = append(out.Segments, seg)
	}
	if err := checkTransmittance(cumT, tr.RayID, "final"); err != nil {
		return out, err
	}
	out.Transmittance = clamp01(cumT)
	out.Radiance = cumL
	return out, nil
}

func (in Integrator) materialAt(ray Ray, p Vec3, step int, ids []string) (sigmaT, source float64) {
	cosTheta := ray.Dir.Neg().Dot(in.Light.Direction.Normalize())
	var shadowSum float64
	for sample := 0; sample < in.ShadowSamples; sample++ {
		if in.Shadow != nil {
			shadowSum += in.Shadow(ray, p, step, sample, ids)
		} else {
			shadowSum += 1
		}
	}
	shadowAvg := 1.0
	if in.ShadowSamples > 0 {
		shadowAvg = shadowSum / float64(in.ShadowSamples)
	}

	for _, id := range ids {
		var m *Medium
		for _, candidate := range in.Media {
			if candidate.ID == id {
				m = candidate
				break
			}
		}
		if m == nil {
			continue
		}
		density := float64(m.Field.At(m.Bounds, p))
		if density <= 0 {
			continue
		}
		sigmaA := density * m.Absorption
		sigmaS := density * m.Scattering
		sigmaT += sigmaA + sigmaS
		phase := PhaseFunction(in.Recipe.Phase, m.Anisotropy, cosTheta)
		ambientPhase := PhaseFunction(PhaseIsotropic, 0, cosTheta)
		source += sigmaS * (in.Light.Ambient*ambientPhase + in.Light.Radiance*shadowAvg*phase)
	}
	return sigmaT, source
}

func checkTransmittance(t float64, rayID, segment string) error {
	const eps = 1e-12
	if math.IsNaN(t) || t < -eps || t > 1+eps {
		return &Error{
			Code: ETrans, RayID: rayID, Segment: segment, Actual: t,
			Field: "transmittance", Message: "transmittance outside [0,1]",
		}
	}
	return nil
}

// AnalyticRay computes the piecewise-constant closed-form reference. It splits
// at every integer-voxel boundary and integrates each homogeneous cell exactly.
// Generated truth rays are shadow-free by construction, so visibility is one.
func AnalyticRay(s *Scenario, ray Ray) (transmittance, radiance, densityIntegral float64, err error) {
	tr := TraceRay(ray, s.Media, s.FixedScale)
	cumT := 1.0
	cumL := 0.0
	byID := map[string]*Medium{}
	for _, m := range s.Media {
		byID[m.ID] = m
	}
	for _, span := range tr.Spans {
		if span.Kind == SpanEmpty {
			continue
		}
		bounds := []float64{span.TMin, span.TMax}
		for _, id := range span.MediumIDs {
			m := byID[id]
			if m == nil {
				continue
			}
			segs := DensitySegments(m, ray, span.TMin, span.TMax)
			for _, ds := range segs {
				bounds = append(bounds, ds.TMin, ds.TMax)
			}
		}
		bounds = uniqueFloat(bounds)
		for i := 0; i+1 < len(bounds); i++ {
			t0, t1 := bounds[i], bounds[i+1]
			if t1-t0 < 1e-12 {
				continue
			}
			mid := (t0 + t1) / 2
			p := ray.Point(mid)
			var sigmaT, source, rawDensityIntegral float64
			for _, id := range span.MediumIDs {
				m := byID[id]
				if m == nil {
					continue
				}
				d := float64(m.Field.At(m.Bounds, p))
				rawDensityIntegral += d * (t1 - t0)
				sigmaT += d * (m.Absorption + m.Scattering)
				cosTheta := ray.Dir.Neg().Dot(s.Light.Direction.Normalize())
				phase := PhaseFunction(s.Recipe.Phase, m.Anisotropy, cosTheta)
				ambientPhase := PhaseFunction(PhaseIsotropic, 0, cosTheta)
				source += d * m.Scattering * (s.Light.Ambient*ambientPhase + s.Light.Radiance*phase)
			}
			densityIntegral += rawDensityIntegral
			od := sigmaT * (t1 - t0)
			if sigmaT > 1e-15 {
				cumL += cumT * (source / sigmaT) * (-math.Expm1(-od))
			}
			cumT *= math.Exp(-od)
		}
	}
	if err := checkTransmittance(cumT, ray.ID, "analytic"); err != nil {
		return 0, 0, 0, err
	}
	return clamp01(cumT), cumL, densityIntegral, nil
}

func uniqueFloat(in []float64) []float64 {
	sort.Float64s(in)
	out := in[:0]
	for _, v := range in {
		if len(out) == 0 || math.Abs(out[len(out)-1]-v) > 1e-10 {
			out = append(out, v)
		}
	}
	return out
}
