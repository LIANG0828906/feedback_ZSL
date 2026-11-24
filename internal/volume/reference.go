package volume

import (
	"math"

	"volumediag/internal/model"
)

// ReferenceRay is the generator's independent truth path. It uses the closed
// form for homogeneous extinction and composite Simpson integration of the
// constructed single-scattering field. It never consumes renderer output.
func ReferenceRay(scene *model.Scene, origin, dir model.Vec3, tNear, tFar float64, steps int) RayResult {
	if steps < 2 {
		steps = 2
	}
	if steps%2 != 0 {
		steps++
	}
	dir, _ = dir.Normalize()
	h := (tFar - tNear) / float64(steps)
	acc := model.Color{}
	used := make(map[string]struct{})
	for i := 0; i <= steps; i++ {
		t := tNear + float64(i)*h
		f, lights := referenceIntegrand(scene, origin, dir, t)
		for _, id := range lights {
			used[id] = struct{}{}
		}
		w := 1.0
		if i == 0 || i == steps {
			w = 1
		} else if i%2 == 1 {
			w = 4
		} else {
			w = 2
		}
		acc = acc.Add(f.Mul(w))
	}
	acc = acc.Mul(h / 3)
	return RayResult{
		Transmittance: ExpTransmittance(scene.Medium.Extinction, tFar-tNear),
		Scatter:       acc,
		Samples:       steps + 1,
		ActualStep:    h,
		UsedLights:    SortStrings(mapKeys(used)),
	}
}

func referenceIntegrand(scene *model.Scene, origin, dir model.Vec3, t float64) (model.Color, []string) {
	p := origin.Add(dir.Mul(t))
	trans := ExpTransmittance(scene.Medium.Extinction, t)
	sum := model.Color{}
	used := make([]string, 0, len(scene.Lights))
	for li := range scene.Lights {
		light := &scene.Lights[li]
		emit := SampleEmission(scene, light, p, dir, scene.Medium.Scattering)
		if emit.Max() <= 0 {
			continue
		}
		used = append(used, light.ID)
		sum[0] += trans[0] * emit[0]
		sum[1] += trans[1] * emit[1]
		sum[2] += trans[2] * emit[2]
	}
	return sum, used
}

func ReferencePixel(scene *model.Scene, cameraIndex, x, y, steps int) (RayResult, bool) {
	if cameraIndex < 0 || cameraIndex >= len(scene.CameraPath) {
		return RayResult{}, false
	}
	camera := &scene.CameraPath[cameraIndex]
	frame, ok := NewCameraFrame(camera, scene.Viewport.Width, scene.Viewport.Height)
	if !ok {
		return RayResult{}, false
	}
	origin, dir := frame.Ray(camera, scene.Viewport.Width, scene.Viewport.Height, x, y)
	return ReferenceRay(scene, origin, dir, 0, camera.Far, steps), true
}

func ReferenceStepsForFar(far float64) int {
	steps := int(math.Ceil(far / 0.004))
	if steps < 1024 {
		steps = 1024
	}
	if steps%2 != 0 {
		steps++
	}
	return steps
}
