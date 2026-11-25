package fog

import (
	"context"
	"encoding/json"
	"math"
)

type SegmentResult struct {
	Segment             Segment `json:"segment"`
	Steps               int     `json:"steps"`
	ShadowSamples       int     `json:"shadow_samples"`
	ShadowVisibility    float64 `json:"shadow_visibility"`
	Source              float64 `json:"source"`
	TransmittanceStart  float64 `json:"transmittance_start"`
	TransmittanceEnd    float64 `json:"transmittance_end"`
	Scattered           float64 `json:"scattered"`
	CumulativeScattered float64 `json:"cumulative_scattered"`
	CacheKey            string  `json:"cache_key"`
}

type RayResult struct {
	RayID              string          `json:"ray_id"`
	X                  int             `json:"x"`
	Y                  int             `json:"y"`
	Radiance           float64         `json:"radiance"`
	Transmittance      float64         `json:"transmittance"`
	Segments           []SegmentResult `json:"segments"`
	Steps              int             `json:"steps"`
	ShadowSamples      int             `json:"shadow_samples"`
	IntersectionDigest string          `json:"intersection_digest"`
	Digest             string          `json:"digest"`
	CacheKey           string          `json:"cache_key"`
}

type IntegrateOptions struct {
	Steps         int
	ShadowSamples int
	TileID        string
	Profile       string
	Budget        *shadowBudget
}

type integrand struct {
	extinction float64
	source     float64
}

func IntegrateRay(ctx context.Context, scene *Scene, ray Ray, in Intersection, opts IntegrateOptions) (RayResult, *Fault) {
	res := RayResult{RayID: ray.ID, X: ray.X, Y: ray.Y, Transmittance: 1}
	if opts.Steps <= 0 || len(in.Segments) == 0 {
		res.IntersectionDigest = in.Digest
		res.Digest = rayResultDigest(res, in.Digest)
		res.CacheKey = ComputeCacheKey(scene.Hash(), ray.ID, rayCacheParamVersion(scene), in.Digest)
		return res, nil
	}
	stepsPerSegment := apportionSteps(opts.Steps, in.Segments, in.Scale)
	shadowPerSegment := apportionShadowSamples(opts.ShadowSamples, in.Segments, in.Scale)
	cumulative := 0.0
	trans := 1.0
	shadowTotal := 0
	for si, seg := range in.Segments {
		n := stepsPerSegment[si]
		length := seg.Length(in.Scale)
		if n <= 0 || length <= 0 {
			continue
		}
		h := length / float64(n)
		mid := FixedToFloat(seg.T0, in.Scale) + length*0.5
		pMid := ray.PointAt(mid)
		shadowN := shadowPerSegment[si]
		if shadowN < 0 {
			shadowN = 0
		}
		visibility := 1.0
		if shadowN > 0 && opts.Budget != nil {
			actual, err := opts.Budget.acquire(ctx, shadowN, opts.TileID, ray.ID)
			if err != nil {
				return res, &Fault{Code: ECanceled, Path: "integration.shadow_budget", TileID: opts.TileID, RayID: ray.ID, Message: "shadow budget wait canceled", Cause: err.Error()}
			}
			shadowN = actual
		}
		if shadowN > 0 {
			visibility = shadowVisibility(scene, pMid, scene.Recipe.LightDirection.Normalize(), scene.Recipe.ShadowDistance, shadowN)
			shadowTotal += shadowN
			if opts.Budget != nil {
				opts.Budget.release(shadowN)
			}
		}
		segStartT := trans
		segScatter := 0.0
		for j := 0; j < n; j++ {
			ts := FixedToFloat(seg.T0, in.Scale) + float64(j)*h
			p := ray.PointAt(ts)
			f := sampleIntegrand(scene, ray, p, seg.ActiveMedia, visibility)
			// Left-endpoint transmittance and source form the declared
			// quadrature. For a homogeneous segment its error decreases
			// monotonically as the step count grows.
			contrib := trans * f.source * h
			segScatter += contrib
			cumulative += contrib
			trans *= math.Exp(-f.extinction * h)
			if trans < 1e-15 {
				trans = 1e-15
			}
		}
		res.Steps += n
		res.Segments = append(res.Segments, SegmentResult{
			Segment: seg, Steps: n, ShadowSamples: shadowN, ShadowVisibility: visibility,
			Source:             sampleIntegrand(scene, ray, pMid, seg.ActiveMedia, visibility).source,
			TransmittanceStart: segStartT, TransmittanceEnd: trans,
			Scattered: segScatter, CumulativeScattered: cumulative,
		})
	}
	res.Transmittance = trans
	res.Radiance = cumulative
	res.ShadowSamples = shadowTotal
	res.IntersectionDigest = in.Digest
	res.Digest = rayResultDigest(res, in.Digest)
	res.CacheKey = ComputeCacheKey(scene.Hash(), ray.ID, rayCacheParamVersion(scene), in.Digest)
	return res, nil
}

func apportionSteps(steps int, segments []Segment, scale int64) []int {
	out := make([]int, len(segments))
	if steps <= 0 || len(segments) == 0 {
		return out
	}
	// A segment boundary is mandatory: merging distinct coefficient sets
	// would change the integral. If the caller supplies fewer primary samples
	// than boundaries, one sample per segment is the minimum and ActualSteps
	// honestly reports the mandatory overrun.
	if steps < len(segments) {
		for i := range out {
			out[i] = 1
		}
		return out
	}
	lengths := make([]float64, len(segments))
	total := 0.0
	for i, s := range segments {
		lengths[i] = s.Length(scale)
		total += lengths[i]
	}
	if total <= 0 {
		return out
	}
	remaining := steps
	for i := 0; i < len(segments); i++ {
		remainingSegments := len(segments) - i
		if remainingSegments == 1 {
			out[i] = remaining
			break
		}
		n := int(math.Round(float64(steps) * lengths[i] / total))
		if n < 1 {
			n = 1
		}
		maxForThis := remaining - (remainingSegments - 1)
		if n > maxForThis {
			n = maxForThis
		}
		out[i] = n
		remaining -= n
	}
	return out
}

func apportionShadowSamples(samples int, segments []Segment, scale int64) []int {
	out := make([]int, len(segments))
	if samples <= 0 || len(segments) == 0 {
		return out
	}
	if samples <= len(segments) {
		for i := 0; i < samples; i++ {
			out[i] = 1
		}
		return out
	}
	lengths := make([]float64, len(segments))
	total := 0.0
	for i, s := range segments {
		lengths[i] = s.Length(scale)
		total += lengths[i]
	}
	if total <= 0 {
		return out
	}
	remaining := samples
	for i := 0; i < len(segments); i++ {
		remainingSegments := len(segments) - i
		if remainingSegments == 1 {
			out[i] = remaining
			break
		}
		n := int(math.Round(float64(samples) * lengths[i] / total))
		if n < 1 {
			n = 1
		}
		maxForThis := remaining - (remainingSegments - 1)
		if n > maxForThis {
			n = maxForThis
		}
		out[i] = n
		remaining -= n
	}
	return out
}

func sampleIntegrand(scene *Scene, ray Ray, p Vec3, active []string, visibility float64) integrand {
	scale := scene.Recipe.CoefficientScale
	cosTheta := ray.Direction.Dot(scene.Recipe.LightDirection.Normalize())
	ext := 0.0
	src := 0.0
	for _, id := range active {
		_, m := scene.MediaByID(id)
		if m == nil || !m.Bounds.Contains(p) {
			continue
		}
		frac := float64(m.DensityAtLocal(p)) / float64(DensityMax)
		sa := float64(m.AbsorptionFP) / float64(scale) * frac
		ss := float64(m.ScatteringFP) / float64(scale) * frac
		ext += sa + ss
		src += ss * EvalPhase(scene.Recipe.Phase, cosTheta, m.Anisotropy) * scene.Recipe.LightIntensity * visibility
	}
	return integrand{extinction: ext, source: src}
}

func shadowVisibility(scene *Scene, p, toLight Vec3, distance float64, samples int) float64 {
	if samples <= 0 || distance <= 0 {
		return 1
	}
	h := distance / float64(samples)
	tau := 0.0
	scale := scene.Recipe.CoefficientScale
	for i := 0; i < samples; i++ {
		q := p.Add(toLight.Mul((float64(i) + 0.5) * h))
		for mi := range scene.Media {
			m := &scene.Media[mi]
			if !m.Bounds.Contains(q) {
				continue
			}
			frac := float64(m.DensityAtLocal(q)) / float64(DensityMax)
			tau += (float64(m.AbsorptionFP) + float64(m.ScatteringFP)) / float64(scale) * frac * h
		}
	}
	v := math.Exp(-tau)
	if v < 1e-12 {
		return 1e-12
	}
	return v
}

func rayResultDigest(res RayResult, intersectionDigest string) string {
	type segDigest struct {
		T0                 int64    `json:"t0"`
		T1                 int64    `json:"t1"`
		Active             []string `json:"active"`
		TransmittanceStart float64  `json:"transmittance_start"`
		TransmittanceEnd   float64  `json:"transmittance_end"`
		Scattered          float64  `json:"scattered"`
	}
	segs := make([]segDigest, 0, len(res.Segments))
	for _, s := range res.Segments {
		segs = append(segs, segDigest{
			T0: s.Segment.T0, T1: s.Segment.T1, Active: s.Segment.ActiveMedia,
			TransmittanceStart: round12(s.TransmittanceStart),
			TransmittanceEnd:   round12(s.TransmittanceEnd),
			Scattered:          round12(s.Scattered),
		})
	}
	b, _ := json.Marshal(struct {
		RayID         string      `json:"ray_id"`
		Radiance      float64     `json:"radiance"`
		Transmittance float64     `json:"transmittance"`
		Intersection  string      `json:"intersection"`
		Segments      []segDigest `json:"segments"`
	}{res.RayID, round12(res.Radiance), round12(res.Transmittance), intersectionDigest, segs})
	return hashStrings(string(b))
}

func round12(v float64) float64 {
	return math.Round(v*1e12) / 1e12
}

func rayCacheParamVersion(scene *Scene) string {
	return hashStrings(scene.CameraHash(), scene.RecipeHash(), scene.Recipe.AlgorithmVersion)
}
