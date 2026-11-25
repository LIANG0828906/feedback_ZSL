package fog

import "math"

var BackgroundRadiance = [3]float64{0.018, 0.028, 0.052}

type IntegrationOptions struct {
	StepBudget   int `json:"step_budget"`
	ShadowBudget int `json:"shadow_budget"`
}

func (o IntegrationOptions) withRecipe(r Recipe) IntegrationOptions {
	if o.StepBudget <= 0 {
		o.StepBudget = r.MaxStepsPerRay
	}
	if o.ShadowBudget <= 0 {
		o.ShadowBudget = r.ShadowBudgetPerRay
	}
	return o
}

func IntegrateRay(scene *Scene, ray Ray, options IntegrationOptions) (*RayResult, error) {
	opts := options.withRecipe(scene.Recipe)
	hits, err := IntersectRayMedia(ray, scene.Media)
	if err != nil {
		return nil, err
	}
	segments, err := BuildRaySegments(ray, hits)
	if err != nil {
		return nil, err
	}
	if len(segments) > opts.StepBudget {
		e := newError(CodeIter, "step budget %d is smaller than ordered segment count %d", opts.StepBudget, len(segments))
		e.RayID = ray.ID
		e.Deviation = float64(len(segments) - opts.StepBudget)
		return nil, e
	}

	allocations := allocateSteps(segments, opts.StepBudget)
	result := &RayResult{RayID: ray.ID, Pixel: ray.Pixel, Segments: make([]SegmentResult, 0, len(segments))}
	cumulativeT := 1.0
	cumulativeScatter := 0.0
	shadowUsed := 0
	shadowExhausted := false

	for segIndex, segment := range segments {
		stepCount := allocations[segIndex]
		near := segment.Near.Fixed.Float64()
		far := segment.Far.Fixed.Float64()
		parameterStep := (far - near) / float64(stepCount)
		segmentT := 1.0
		segmentScatter := 0.0
		segmentShadow := 0

		for step := 0; step < stepCount; step++ {
			t := near + (float64(step)+0.5)*parameterStep
			material := SampleMaterials(scene, ray, t, segment.MediumIDs)
			remainingShadow := opts.ShadowBudget - shadowUsed
			wantShadow := scene.Recipe.ShadowSamplesPerStep
			if wantShadow > remainingShadow {
				wantShadow = remainingShadow
			}
			shadowT := 0.0
			if wantShadow <= 0 {
				shadowExhausted = true
			} else {
				shadow := ShadowTransmittanceBudget(scene, ray.Origin.Add(ray.Dir.Scale(t)), wantShadow)
				shadowT = shadow.Transmittance
				shadowUsed += shadow.Used
				segmentShadow += shadow.Used
				if shadow.Used < wantShadow {
					shadowExhausted = true
				}
			}

			source := material.Ambient + material.PhaseScatter*scene.Light.Intensity*shadowT
			dt := parameterStep * ray.Dir.Len()
			tau := material.SigmaT * dt
			stepT := math.Exp(-tau)
			var scatter float64
			if material.SigmaT > 1e-12 {
				scatter = cumulativeT * source / material.SigmaT * (1 - stepT)
			} else {
				scatter = cumulativeT * source * dt
			}
			segmentScatter += scatter
			cumulativeScatter += scatter
			cumulativeT *= stepT
			segmentT *= stepT
			result.Steps++
		}

		result.Segments = append(result.Segments, SegmentResult{
			SegmentIndex:         segIndex,
			MediumIDs:            append([]string(nil), segment.MediumIDs...),
			Near:                 segment.Near.Fixed,
			Far:                  segment.Far.Fixed,
			Transmittance:        segmentT,
			CumulativeT:          cumulativeT,
			Scattering:           segmentScatter,
			CumulativeScattering: cumulativeScatter,
			Steps:                stepCount,
			ShadowSamples:        segmentShadow,
		})
	}

	result.FinalTransmittance = cumulativeT
	result.Scattering = cumulativeScatter
	result.ShadowSamples = shadowUsed
	result.ShadowExhausted = shadowExhausted
	result.Radiance = RadianceFor(scene, cumulativeT, cumulativeScatter)
	result.Color = RadianceToColor(result.Radiance)
	result.CacheKey = RayCacheKey(scene, ray, opts, segments)
	return result, nil
}

func RadianceFor(scene *Scene, transmittance, scattering float64) [3]float64 {
	tint := scene.Light.Color
	return [3]float64{
		BackgroundRadiance[0]*transmittance + scattering*(0.45+0.55*tint[0]),
		BackgroundRadiance[1]*transmittance + scattering*(0.45+0.55*tint[1]),
		BackgroundRadiance[2]*transmittance + scattering*(0.45+0.55*tint[2]),
	}
}

func RadianceToColor(r [3]float64) [3]uint8 {
	var color [3]uint8
	for i, v := range r {
		if math.IsNaN(v) || v < 0 {
			v = 0
		}
		if v > 1 {
			v = 1
		}
		v = math.Pow(v, 1.0/2.2)
		color[i] = uint8(math.Round(v * 255))
	}
	return color
}

func allocateSteps(segments []RaySegment, budget int) []int {
	n := len(segments)
	allocations := make([]int, n)
	if n == 0 {
		return allocations
	}
	if budget <= n {
		for i := range allocations {
			allocations[i] = 1
		}
		return allocations
	}
	totalLength := 0.0
	for _, segment := range segments {
		totalLength += segment.Length
	}
	for i := range allocations {
		allocations[i] = 1
	}
	remainingBudget := budget - n
	type share struct {
		index int
		value float64
	}
	shares := make([]share, 0, n)
	assigned := 0
	for i, segment := range segments {
		raw := 0.0
		if totalLength > 0 {
			raw = float64(remainingBudget) * segment.Length / totalLength
		}
		extra := int(math.Floor(raw))
		allocations[i] += extra
		assigned += extra
		shares = append(shares, share{i, raw - float64(extra)})
	}
	for assigned < remainingBudget {
		idx := 0
		best := -1.0
		for _, s := range shares {
			if s.value > best {
				best = s.value
				idx = s.index
			}
		}
		allocations[idx]++
		shares[idx].value = 0
		assigned++
	}
	return allocations
}
