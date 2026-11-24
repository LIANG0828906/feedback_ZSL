package volume

import (
	"fmt"
	"math"

	"volumediag/internal/diag"
	"volumediag/internal/model"
)

type JitterMode string

const (
	JitterNone   JitterMode = "none"
	JitterHalton JitterMode = "halton"
	JitterHash   JitterMode = "hash"
)

type Params struct {
	Step                float64    `json:"step"`
	Jitter              JitterMode `json:"jitter"`
	JitterSamples       int        `json:"jitter_samples"`
	MaxWorkers          int        `json:"max_workers"`
	MaxSamplesPerThread int        `json:"max_samples_per_thread"`
	BufferSlots         int        `json:"buffer_slots"`
	BatchSize           int        `json:"batch_size"`
	CameraID            string     `json:"camera_id,omitempty"`
	Preview             bool       `json:"preview"`
}

func DefaultParams() Params {
	return Params{
		Step:                0.05,
		Jitter:              JitterNone,
		JitterSamples:       1,
		MaxWorkers:          4,
		MaxSamplesPerThread: 8192,
		BufferSlots:         4,
		BatchSize:           8,
	}
}

func PreviewParams() Params {
	p := DefaultParams()
	p.Step = 0.14
	p.Jitter = JitterHalton
	p.JitterSamples = 2
	return p
}

func (p Params) Validate(scene *model.Scene) error {
	snapshot := p.Snapshot()
	fail := func(field, msg string) error {
		e := diag.Parameter("", field, msg)
		e.Snapshot = snapshot
		return e
	}
	if !isFinite(p.Step) || p.Step < 0.01 || p.Step > 2.0 {
		return fail("params.step", "step must be finite and in [0.01, 2.0]")
	}
	if p.Jitter != JitterNone && p.Jitter != JitterHalton && p.Jitter != JitterHash {
		return fail("params.jitter", "jitter must be none, halton or hash")
	}
	if p.JitterSamples < 1 || p.JitterSamples > 16 {
		return fail("params.jitter_samples", "jitter_samples must be in [1, 16]")
	}
	if p.MaxWorkers < 1 || p.MaxWorkers > 64 {
		return fail("params.max_workers", "max_workers must be in [1, 64]")
	}
	if p.MaxSamplesPerThread < 64 || p.MaxSamplesPerThread > 4_000_000 {
		return fail("params.max_samples_per_thread", "max_samples_per_thread must be in [64, 4000000]")
	}
	if p.BufferSlots < 1 || p.BufferSlots > 64 {
		return fail("params.buffer_slots", "buffer_slots must be in [1, 64]")
	}
	if p.BatchSize < 1 || p.BatchSize > 512 {
		return fail("params.batch_size", "batch_size must be in [1, 512]")
	}
	if scene != nil {
		if len(scene.CameraPath) == 0 {
			return fail("params.camera_id", "scene has no camera for parameter validation")
		}
		cam := &scene.CameraPath[0]
		if p.CameraID != "" {
			found := false
			for i := range scene.CameraPath {
				if scene.CameraPath[i].ID == p.CameraID {
					cam = &scene.CameraPath[i]
					found = true
					break
				}
			}
			if !found {
				return &diag.Error{Code: diag.ERef, Field: "params.camera_id", Message: "camera id does not exist"}
			}
		}
		steps := int(math.Ceil(cam.Far / p.Step))
		if steps < 1 {
			return fail("params.step", "step produces no samples")
		}
	}
	return nil
}

func (p Params) Snapshot() map[string]any {
	return map[string]any{
		"step":                   p.Step,
		"jitter":                 string(p.Jitter),
		"jitter_samples":         p.JitterSamples,
		"max_workers":            p.MaxWorkers,
		"max_samples_per_thread": p.MaxSamplesPerThread,
		"buffer_slots":           p.BufferSlots,
		"batch_size":             p.BatchSize,
		"camera_id":              p.CameraID,
		"preview":                p.Preview,
	}
}

func (p Params) Version() string {
	return model.HashValue(struct {
		Step          float64    `json:"step"`
		Jitter        JitterMode `json:"jitter"`
		JitterSamples int        `json:"jitter_samples"`
		CameraID      string     `json:"camera_id"`
	}{
		Step:          p.Step,
		Jitter:        p.Jitter,
		JitterSamples: p.JitterSamples,
		CameraID:      p.CameraID,
	})
}

func (p Params) ActualStep(far float64) (float64, int) {
	steps := int(math.Ceil(far / p.Step))
	if steps < 1 {
		steps = 1
	}
	return far / float64(steps), steps
}

func (p Params) String() string {
	return fmt.Sprintf("step=%.6g jitter=%s samples=%d", p.Step, p.Jitter, p.JitterSamples)
}

func ErrorBound(scene *model.Scene, camera *model.Camera, p Params) Quality {
	actualStep, steps := p.ActualStep(camera.Far)
	lightCount := len(scene.Lights)
	sigmaTMin := scene.Medium.Extinction[0]
	if scene.Medium.Extinction[1] < sigmaTMin {
		sigmaTMin = scene.Medium.Extinction[1]
	}
	if scene.Medium.Extinction[2] < sigmaTMin {
		sigmaTMin = scene.Medium.Extinction[2]
	}
	var sigmaTIntegral float64
	if sigmaTMin <= 1e-12 {
		sigmaTIntegral = camera.Far
	} else {
		sigmaTIntegral = (1 - math.Exp(-sigmaTMin*camera.Far)) / sigmaTMin
	}
	sigmaSMax := scene.Medium.Scattering.Max()
	var emissionMax float64
	for i := range scene.Lights {
		l := &scene.Lights[i]
		effectiveG := 0.75*l.Anisotropy + 0.25*scene.Medium.PhaseG
		emissionMax += l.Intensity.Max() * PhaseMax(effectiveG)
	}
	bound := actualStep * sigmaSMax * emissionMax * sigmaTIntegral
	samples := steps * p.JitterSamples
	return Quality{
		RequestedStep:      p.Step,
		ActualStep:         actualStep,
		StepsPerRay:        steps,
		JitterSamples:      p.JitterSamples,
		SamplesPerPixel:    samples,
		LightCount:         lightCount,
		SigmaTMin:          sigmaTMin,
		ErrorBoundPerBlock: bound,
		ErrorBoundPerPixel: bound,
		Formula:            "DeltaT * sigma_s,max * sum_l(I_l,max * phaseMax(g_l)) * integral_0^far(exp(-sigma_t,min*t)dt)",
	}
}

func isFinite(v float64) bool { return !math.IsNaN(v) && !math.IsInf(v, 0) }
