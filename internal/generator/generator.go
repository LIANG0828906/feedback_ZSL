package generator

import (
	"fmt"
	"math"
	"math/rand"

	"volumekernel/internal/model"
)

const DefaultSeed int64 = 20261127

func Generate(seed int64) model.Scene {
	rng := rand.New(rand.NewSource(seed))
	media := []model.Medium{
		newMedium(
			"media-alpha",
			model.Vec3{X: -2.20, Y: -1.40, Z: -1.20},
			model.Vec3{X: 1.80, Y: 1.60, Z: 0.90},
			0.22, 0.16, 0.15, 6, 6, 5, rng,
		),
		newMedium(
			"media-beta",
			model.Vec3{X: -1.00, Y: -0.80, Z: -0.70},
			model.Vec3{X: 2.40, Y: 1.90, Z: 1.40},
			0.12, 0.20, -0.25, 5, 6, 6, rng,
		),
		newMedium(
			"media-gamma",
			model.Vec3{X: -0.60, Y: 0.10, Z: -1.50},
			model.Vec3{X: 2.00, Y: 2.00, Z: -0.20},
			0.18, 0.10, 0.55, 6, 4, 5, rng,
		),
	}
	cameras := model.CameraTrack{
		ID: "camera-orbit",
		Samples: []model.CameraSample{
			newCamera("cam-00", 0.00, -0.55, 0.10, 9.30, 0.00, 0.20, -0.10, 55),
			newCamera("cam-01", 0.25, -0.30, 0.24, 8.95, 0.00, 0.15, -0.05, 56),
			newCamera("cam-02", 0.50, 0.00, 0.38, 8.60, 0.05, 0.20, 0.00, 57),
			newCamera("cam-03", 0.75, 0.32, 0.20, 8.90, 0.10, 0.15, 0.05, 56),
			newCamera("cam-04", 1.00, 0.60, 0.08, 9.35, 0.00, 0.18, 0.08, 55),
		},
	}
	for i := range cameras.Samples {
		cameras.Samples[i].Aspect = 48.0 / 30.0
	}
	s := model.Scene{
		ID:               "realtime-transparent-media",
		Version:          fmt.Sprintf("scene-seed-%d-v1", seed),
		Seed:             seed,
		FixedScale:       model.FixedScale,
		CoefficientScale: model.FixedScale,
		Media:            media,
		CameraTrack:      cameras,
		Viewport:         model.Viewport{Width: 48, Height: 30},
		Recipe: model.Recipe{
			Version:              "recipe-balanced-v1",
			Steps:                192,
			Phase:                model.PhaseHG,
			ShadowSamples:        16,
			ShadowBudget:         240_000,
			BlockWidth:           8,
			BlockHeight:          8,
			MaxParallelBlocks:    3,
			MaxRaysPerBlock:      32,
			PreviewSteps:         24,
			PreviewShadowSamples: 4,
			PreviewLatencyMS:     250,
			PixelTolerance:       0.004,
			OverlapMode:          "ordered-composite",
		},
		Light: model.DirectionalLight{
			Direction:         model.Vec3{X: 0.30, Y: 0.45, Z: -0.84}.Normalize(),
			Radiance:          1.35,
			ShadowTau:         0.65,
			ShadowProfileGain: 0.35,
		},
	}
	return s
}

func newMedium(id string, min, max model.Vec3, absorption, scattering, anisotropy float64, nx, ny, nz int, rng *rand.Rand) model.Medium {
	m := model.Medium{
		ID:         id,
		Bounds:     model.AABB{Min: min, Max: max},
		Absorption: absorption,
		Scattering: scattering,
		Anisotropy: anisotropy,
		Density:    model.DensityField{NX: nx, NY: ny, NZ: nz},
	}
	m.Density.Values = make([]uint8, m.Density.Len())
	for k := 0; k < nz; k++ {
		for j := 0; j < ny; j++ {
			for i := 0; i < nx; i++ {
				u := (float64(i) + 0.5) / float64(nx)
				v := (float64(j) + 0.5) / float64(ny)
				w := (float64(k) + 0.5) / float64(nz)
				base := 0.46 +
					0.20*math.Sin(2.2*u+1.1*w) +
					0.15*math.Cos(1.7*v-0.9*w) +
					0.08*(rng.Float64()-0.5)
				if id == "media-beta" {
					base += 0.08 * u
				}
				if id == "media-gamma" {
					base -= 0.06 * v
				}
				m.Density.Values[(k*ny+j)*nx+i] = uint8(model.ClipUnit(base))
			}
		}
	}
	h, _ := model.HashValue(struct {
		Absorption float64
		Scattering float64
		Anisotropy float64
		Density    model.DensityField
	}{absorption, scattering, anisotropy, m.Density})
	m.MaterialVersion = "mat-" + h[:16]
	return m
}

func newCamera(id string, t, px, py, pz, tx, ty, tz, fov float64) model.CameraSample {
	return model.CameraSample{
		ID:       id,
		Time:     t,
		Position: model.Vec3{X: px, Y: py, Z: pz},
		Target:   model.Vec3{X: tx, Y: ty, Z: tz},
		Up:       model.Vec3{X: 0, Y: 1, Z: 0},
		FOVDeg:   fov,
		Aspect:   48.0 / 30.0,
	}
}

func CloneScene(s model.Scene) model.Scene {
	cp := s
	cp.Media = append([]model.Medium(nil), s.Media...)
	for i := range cp.Media {
		cp.Media[i].Density.Values = append([]uint8(nil), s.Media[i].Density.Values...)
	}
	cp.CameraTrack.Samples = append([]model.CameraSample(nil), s.CameraTrack.Samples...)
	return cp
}
