package kernel

import (
	"math"
	"sort"

	"volumekernel/internal/camera"
	"volumekernel/internal/geom"
	"volumekernel/internal/model"
	"volumekernel/internal/physics"
)

type Options struct {
	Steps             int
	ShadowSamples     int
	StepBudget        int
	ShadowSampleLimit int
}

func RenderRay(s model.Scene, sample model.CameraSample, vp model.Viewport, x, y int, opts Options) (model.PixelResult, []string, error) {
	ray, err := camera.RayForPixel(sample, vp, x, y)
	if err != nil {
		return model.PixelResult{}, nil, model.ParamError("", "camera", "%v", err)
	}
	deps := Dependencies(s, ray)
	spans, err := BuildOrderedSpans(s, ray)
	if err != nil {
		return model.PixelResult{}, deps, err
	}
	res := model.PixelResult{
		X:             x,
		Y:             y,
		RayID:         ray.ID,
		Transmittance: 1,
		Segments:      make([]model.RaySegment, 0, len(spans)),
	}
	if len(spans) == 0 {
		return res, deps, nil
	}
	alloc, err := AllocateSteps(spans, opts.Steps)
	if err != nil {
		e := model.NewError(model.EBudget, "step budget cannot be apportioned")
		e.RayID = ray.ID
		e.Actual = len(spans)
		e.Limit = opts.Steps
		e.Cause = err
		return res, deps, e
	}
	actualSteps := 0
	for _, n := range alloc {
		actualSteps += n
	}
	if opts.StepBudget > 0 && actualSteps > opts.StepBudget {
		e := model.NewError(model.EBudget, "ray step budget exceeded")
		e.RayID = ray.ID
		e.Actual = actualSteps
		e.Limit = opts.StepBudget
		return res, deps, e
	}
	actualShadow := 0
	if opts.ShadowSamples > 0 {
		actualShadow = actualSteps * opts.ShadowSamples
	}
	if opts.ShadowSampleLimit > 0 && actualShadow > opts.ShadowSampleLimit {
		e := model.NewError(model.EBudget, "ray shadow-sample budget exceeded")
		e.RayID = ray.ID
		e.Actual = actualShadow
		e.Limit = opts.ShadowSampleLimit
		return res, deps, e
	}
	shadow := physics.EstimateShadow(s.Light, opts.ShadowSamples)
	cosTheta := physics.CosTheta(ray.Direction, s.Light.Direction)
	trans := 1.0
	scattered := 0.0
	scale := float64(s.FixedScale)
	for si, sp := range spans {
		n := alloc[si]
		t0 := float64(sp.T0) / scale
		t1 := float64(sp.T1) / scale
		dt := (t1 - t0) / float64(n)
		segT := 1.0
		segScatter := 0.0
		for k := 0; k < n; k++ {
			tm := t0 + (float64(k)+0.5)*dt
			p := ray.At(tm)
			sigmaT, sigmaS, g := sampleCoefficients(s, sp.MediaIndexes, p)
			if sigmaT <= 0 {
				continue
			}
			stepT := math.Exp(-sigmaT * dt)
			if sigmaS > 0 {
				phase := physics.Phase(s.Recipe.Phase, g, cosTheta)
				segScatter += trans * phase * s.Light.Radiance * shadow * sigmaS * dt
			}
			trans *= stepT
			segT *= stepT
		}
		scattered += segScatter
		res.Segments = append(res.Segments, model.RaySegment{
			Order:             si,
			T0Ticks:           sp.T0,
			T1Ticks:           sp.T1,
			T0ResidualTicks:   sp.T0ResidualTicks,
			T1ResidualTicks:   sp.T1ResidualTicks,
			MediaIDs:          append([]string(nil), sp.MediaIDs...),
			Steps:             n,
			ShadowSamples:     n * opts.ShadowSamples,
			Transmittance:     segT,
			ScatteredRadiance: segScatter,
		})
	}
	res.Transmittance = trans
	res.ScatteredRadiance = scattered
	res.Steps = actualSteps
	res.ShadowSamples = actualShadow
	return res, deps, nil
}

func sampleCoefficients(s model.Scene, indexes []int, p model.Vec3) (sigmaT, sigmaS, g float64) {
	for _, mi := range indexes {
		m := &s.Media[mi]
		d := m.DensityAt(p)
		if d <= 0 {
			continue
		}
		sT := d * (m.Absorption + m.Scattering)
		sS := d * m.Scattering
		sigmaT += sT
		sigmaS += sS
		g += sS * m.Anisotropy
	}
	if sigmaS > 0 {
		g /= sigmaS
	}
	return
}

func Dependencies(s model.Scene, ray camera.Ray) []string {
	set := make(map[string]struct{})
	for mi := range s.Media {
		m := &s.Media[mi]
		iv, hit := geom.RayAABB(ray.Origin, ray.Direction, m.Bounds)
		if !hit {
			continue
		}
		set["coeff:"+m.ID+":absorption"] = struct{}{}
		set["coeff:"+m.ID+":scattering"] = struct{}{}
		set["medium:"+m.ID] = struct{}{}
		for k := 0; k < m.Density.NZ; k++ {
			for j := 0; j < m.Density.NY; j++ {
				for i := 0; i < m.Density.NX; i++ {
					vi, ok := geom.RayVoxel(ray.Origin, ray.Direction, *m, i, j, k)
					if !ok {
						continue
					}
					t0 := math.Max(iv.T0, vi.T0)
					t1 := math.Min(iv.T1, vi.T1)
					if t1 <= t0 {
						continue
					}
					set["voxel:"+m.ID+":"+itoa(i)+","+itoa(j)+","+itoa(k)] = struct{}{}
				}
			}
		}
	}
	out := make([]string, 0, len(set))
	for id := range set {
		out = append(out, id)
	}
	sort.Strings(out)
	return out
}

func itoa(v int) string {
	if v == 0 {
		return "0"
	}
	buf := [20]byte{}
	i := len(buf)
	neg := v < 0
	if neg {
		v = -v
	}
	for v > 0 {
		i--
		buf[i] = byte('0' + v%10)
		v /= 10
	}
	if neg {
		i--
		buf[i] = '-'
	}
	return string(buf[i:])
}
