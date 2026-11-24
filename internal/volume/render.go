package volume

import (
	"context"
	"hash/fnv"
	"math"

	"volumediag/internal/diag"
	"volumediag/internal/model"
)

type sampleValue struct {
	scatter model.Color
}

func RenderBlock(ctx context.Context, scene *model.Scene, cameraIndex int, p Params, bx, by int) (*BlockResult, *diag.Error) {
	if cameraIndex < 0 || cameraIndex >= len(scene.CameraPath) {
		return nil, &diag.Error{Code: diag.ERef, Field: "camera_index", Message: "camera index out of range"}
	}
	camera := &scene.CameraPath[cameraIndex]
	frame, ok := NewCameraFrame(camera, scene.Viewport.Width, scene.Viewport.Height)
	if !ok {
		return nil, &diag.Error{Code: diag.EPara, Field: "camera", Message: "cannot construct camera basis"}
	}
	x0 := bx * scene.BlockSize.Width
	y0 := by * scene.BlockSize.Height
	x1 := minInt(x0+scene.BlockSize.Width, scene.Viewport.Width)
	y1 := minInt(y0+scene.BlockSize.Height, scene.Viewport.Height)
	if x0 >= x1 || y0 >= y1 {
		return nil, &diag.Error{Code: diag.EDep, BlockX: bx, BlockY: by, Message: "empty block range"}
	}
	result := &BlockResult{
		Key: BlockKey{
			CameraIndex: cameraIndex,
			CameraID:    camera.ID,
			X:           bx,
			Y:           by,
		},
		Width:            x1 - x0,
		Height:           y1 - y0,
		ActualStep:       0,
		Jitter:           string(p.Jitter),
		JitterSamples:    p.JitterSamples,
		ParamsVersion:    p.Version(),
		AlgorithmVersion: AlgorithmVersion,
	}
	result.Pixels = make([]PixelResult, 0, result.Width*result.Height)
	result.Composite = make([]model.Color, 0, result.Width*result.Height)
	usedLights := make(map[string]struct{})
	usedOccluders := make(map[string]struct{})
	var sumT, sumScatter, maxScatter model.Color
	actualStep := 0.0
	samplesPerPixel := 0
	pixelCount := 0
	for y := y0; y < y1; y++ {
		for x := x0; x < x1; x++ {
			if err := ctx.Err(); err != nil {
				return nil, &diag.Error{Code: diag.ECanceled, Message: "render canceled", Cause: err}
			}
			origin, dir := frame.Ray(camera, scene.Viewport.Width, scene.Viewport.Height, x, y)
			ray, err := renderRay(ctx, scene, frame, camera, origin, dir, 0, camera.Far, x, y, p)
			if err != nil {
				err.BlockX = bx
				err.BlockY = by
				err.PixelX = x
				err.PixelY = y
				return nil, err
			}
			actualStep = ray.ActualStep
			samplesPerPixel = ray.Samples
			pixel := PixelResult{
				X:             x,
				Y:             y,
				Transmittance: ray.Transmittance,
				Scatter:       ray.Scatter,
				Volume:        ray.Scatter,
				UsedLights:    ray.UsedLights,
			}
			result.Pixels = append(result.Pixels, pixel)
			for _, id := range ray.UsedLights {
				usedLights[id] = struct{}{}
				if light, idx := scene.FindLight(id); idx >= 0 {
					for _, occ := range light.OccluderIDs {
						usedOccluders[occ] = struct{}{}
					}
				}
			}
			sumT = sumT.Add(ray.Transmittance)
			sumScatter = sumScatter.Add(ray.Scatter)
			maxScatter = model.Color{
				math.Max(maxScatter[0], ray.Scatter[0]),
				math.Max(maxScatter[1], ray.Scatter[1]),
				math.Max(maxScatter[2], ray.Scatter[2]),
			}
			pixelCount++
		}
	}
	if pixelCount > 0 {
		result.Stats = BlockStats{
			MeanTransmittance: sumT.Mul(1 / float64(pixelCount)),
			MeanScatter:       sumScatter.Mul(1 / float64(pixelCount)),
			MaxScatter:        maxScatter,
			SamplesPerPixel:   samplesPerPixel,
			PixelCount:        pixelCount,
		}
	}
	result.UsedLights = SortStrings(mapKeys(usedLights))
	result.UsedOccluders = SortStrings(mapKeys(usedOccluders))
	result.ActualStep = actualStep
	result.Composite = make([]model.Color, len(result.Pixels))
	for i := range result.Pixels {
		base := GeometryColor(scene, cameraIndex, result.Pixels[i].X, result.Pixels[i].Y)
		result.Composite[i] = base.Add(result.Pixels[i].Volume)
	}
	result.Digest = result.ComputeDigest()
	return result, nil
}

func RenderRayPublic(ctx context.Context, scene *model.Scene, cameraIndex int, origin, dir model.Vec3, tNear, tFar float64, pixelX, pixelY int, p Params) (RayResult, *diag.Error) {
	if cameraIndex < 0 || cameraIndex >= len(scene.CameraPath) {
		return RayResult{}, &diag.Error{Code: diag.ERef, Field: "camera_index", Message: "camera index out of range"}
	}
	camera := &scene.CameraPath[cameraIndex]
	frame, ok := NewCameraFrame(camera, scene.Viewport.Width, scene.Viewport.Height)
	if !ok {
		return RayResult{}, &diag.Error{Code: diag.EPara, Field: "camera", Message: "cannot construct camera basis"}
	}
	return renderRay(ctx, scene, frame, camera, origin, dir, tNear, tFar, pixelX, pixelY, p)
}

func renderRay(ctx context.Context, scene *model.Scene, frame CameraFrame, camera *model.Camera,
	origin, dir model.Vec3, tNear, tFar float64, pixelX, pixelY int, p Params) (RayResult, *diag.Error) {
	length := tFar - tNear
	if length <= 0 {
		return RayResult{}, &diag.Error{Code: diag.EPara, Field: "t_far", Message: "ray interval must be positive"}
	}
	actualStep, steps := p.ActualStep(length)
	unitsPerStep := p.JitterSamples
	totalSamples := steps * unitsPerStep
	chunk := p.MaxSamplesPerThread
	if chunk < 1 {
		chunk = 64
	}
	if chunk > 2048 {
		chunk = 2048
	}
	buf := make([]sampleValue, minInt(totalSamples, chunk))
	acc := model.Color{}
	used := make(map[string]struct{})
	seed := hashSeed(scene.Seed, camera.ID, pixelX, pixelY)
	processed := 0
	for processed < totalSamples {
		if err := ctx.Err(); err != nil {
			return RayResult{}, &diag.Error{Code: diag.ECanceled, Message: "render canceled", Cause: err}
		}
		n := minInt(len(buf), totalSamples-processed)
		for i := 0; i < n; i++ {
			idx := processed + i
			stepIndex := idx / unitsPerStep
			subIndex := idx % unitsPerStep
			stepStart := tNear + float64(stepIndex)*actualStep
			stepEnd := math.Min(tNear+float64(stepIndex+1)*actualStep, tFar)
			dt := stepEnd - stepStart
			if dt <= 0 {
				buf[i] = sampleValue{}
				continue
			}
			frac := sampleFraction(p.Jitter, scene.Seed, camera.ID, pixelX, pixelY,
				seed, idx, stepIndex, subIndex, p.JitterSamples)
			t := stepStart + frac*dt
			samplePos := origin.Add(dir.Mul(t))
			trans := ExpTransmittance(scene.Medium.Extinction, t)
			scatter := model.Color{}
			for li := range scene.Lights {
				light := &scene.Lights[li]
				emit := SampleEmission(scene, light, samplePos, dir, scene.Medium.Scattering)
				if emit.Max() <= 0 {
					continue
				}
				used[light.ID] = struct{}{}
				scatter[0] += trans[0] * emit[0]
				scatter[1] += trans[1] * emit[1]
				scatter[2] += trans[2] * emit[2]
			}
			weight := dt / float64(unitsPerStep)
			buf[i] = sampleValue{
				scatter: scatter.Mul(weight),
			}
		}
		for i := 0; i < n; i++ {
			acc = acc.Add(buf[i].scatter)
		}
		processed += n
	}
	return RayResult{
		Transmittance: ExpTransmittance(scene.Medium.Extinction, length),
		Scatter:       acc,
		Samples:       totalSamples,
		ActualStep:    actualStep,
		UsedLights:    SortStrings(mapKeys(used)),
	}, nil
}

func sampleFraction(jitter JitterMode, seed int64, cameraID string, x, y int, hash uint64, idx, step, sub, samples int) float64 {
	base := (float64(sub) + 0.5) / float64(samples)
	switch jitter {
	case JitterHalton:
		h := halton(idx+1, 2)
		offset := float64(hash%10007) / 10007.0
		h = math.Mod(h+offset, 1)
		return (float64(sub) + h) / float64(samples)
	case JitterHash:
		h := unitHash(hash, uint64(idx+1))
		return (float64(sub) + h) / float64(samples)
	default:
		return base
	}
}

func hashSeed(seed int64, cameraID string, x, y int) uint64 {
	h := fnv.New64a()
	_, _ = h.Write([]byte(cameraID))
	var b [24]byte
	put := func(off int, v uint64) {
		for i := 0; i < 8; i++ {
			b[off+i] = byte(v >> (8 * i))
		}
	}
	put(0, uint64(seed))
	put(8, uint64(int64(x)))
	put(16, uint64(int64(y)))
	_, _ = h.Write(b[:])
	return h.Sum64()
}

func unitHash(seed, idx uint64) float64 {
	x := seed + 0x9e3779b97f4a7c15 + idx*0xbf58476d1ce4e5b9
	x ^= x >> 30
	x *= 0xbf58476d1ce4e5b9
	x ^= x >> 27
	x *= 0x94d049bb133111eb
	x ^= x >> 31
	return float64(x>>11) / float64(uint64(1)<<53)
}

func halton(index, base int) float64 {
	f := 1.0
	r := 0.0
	i := index
	for i > 0 {
		f /= float64(base)
		r += f * float64(i%base)
		i /= base
	}
	return r
}

// GeometryColor is the deterministic screen-space geometry pass used as the
// additive blend base. It is intentionally independent of the volume cache.
func GeometryColor(scene *model.Scene, cameraIndex, x, y int) model.Color {
	if cameraIndex < 0 || cameraIndex >= len(scene.CameraPath) {
		return model.Color{0.01, 0.012, 0.016}
	}
	camera := &scene.CameraPath[cameraIndex]
	frame, ok := NewCameraFrame(camera, scene.Viewport.Width, scene.Viewport.Height)
	if !ok {
		return model.Color{0.01, 0.012, 0.016}
	}
	origin, dir := frame.Ray(camera, scene.Viewport.Width, scene.Viewport.Height, x, y)
	best := math.Inf(1)
	var color model.Color
	for i := range scene.Geometry {
		hit, ok := MeshIntersect(&scene.Geometry[i], origin, dir, camera.Far)
		if ok && hit.T < best {
			best = hit.T
			color = hit.Color
		}
	}
	if math.IsInf(best, 1) {
		checker := 0.0
		if (x/4+y/4)%2 == 0 {
			checker = 0.006
		}
		return model.Color{0.012 + checker, 0.016 + checker, 0.024 + checker}
	}
	depthFade := math.Exp(-best / math.Max(camera.Far, 1))
	return color.Mul(0.35 + 0.65*depthFade)
}

func mapKeys(m map[string]struct{}) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	return out
}

func minInt(a, b int) int {
	if a < b {
		return a
	}
	return b
}
