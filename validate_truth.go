package volumefog

import "math"

type TruthTolerance struct {
	Transmittance float64 `json:"transmittance"`
	Radiance      float64 `json:"radiance"`
	Color         float64 `json:"color"`
}

func DefaultTruthTolerance() TruthTolerance {
	return TruthTolerance{Transmittance: 2e-7, Radiance: 2e-7, Color: 2e-7}
}

// ValidateTruth compares full-budget integration against the generator's
// analytic truth. The first violation is returned as E_INT with its RayID and
// measured deviation.
func ValidateTruth(scene *Scene) error {
	if scene == nil || scene.Truth == nil {
		return schemaErr("", "truth", "scene has no generated truth")
	}
	tol := DefaultTruthTolerance()
	sample := map[string]Ray{}
	for _, r := range SampleRays(scene) {
		sample[r.ID] = r
	}
	checkRay := func(ray Ray, want RayTruth) error {
		got, err := IntegrateRay(scene, ray, IntegrateOptions{
			Steps: scene.Recipe.Steps, ShadowSamples: scene.Recipe.ShadowSamples,
		})
		if err != nil {
			return err
		}
		if len(got.Segments) != len(want.Segments) {
			return &FogError{Code: E_INT, Message: "truth segment count mismatch", RayID: ray.ID,
				Deviation: floatPtr(math.Abs(float64(len(got.Segments) - len(want.Segments))))}
		}
		for i := range want.Segments {
			g, w := got.Segments[i], want.Segments[i]
			if g.T0Q != w.T0Q || g.T1Q != w.T1Q || !sameStrings(g.Active, w.Active) {
				return &FogError{Code: E_INT, Message: "ray segment boundary/order mismatch",
					RayID: ray.ID, Field: "segment[" + itoa(i) + "]",
					Actual: floatPtr(float64(g.T0Q)), Expected: floatPtr(float64(w.T0Q))}
			}
			dt := math.Abs(g.Transmittance - w.Transmittance)
			dl := math.Abs(g.CumulativeRadiance - w.CumulativeRadiance)
			if dt > tol.Transmittance || dl > tol.Radiance {
				return &FogError{Code: E_INT, Message: "ray segment truth deviation exceeds tolerance",
					RayID: ray.ID, Field: "segment[" + itoa(i) + "]",
					Deviation: floatPtr(math.Max(dt, dl)), Tolerance: floatPtr(math.Max(tol.Transmittance, tol.Radiance))}
			}
		}
		dt := math.Abs(got.FinalTransmittance - want.FinalTransmittance)
		dl := math.Abs(got.FinalRadiance - want.FinalRadiance)
		if dt > tol.Transmittance || dl > tol.Radiance {
			return &FogError{Code: E_INT, Message: "ray truth deviation exceeds tolerance",
				RayID: ray.ID, Deviation: floatPtr(math.Max(dt, dl)),
				Tolerance: floatPtr(math.Max(tol.Transmittance, tol.Radiance))}
		}
		return nil
	}
	for _, want := range scene.Truth.Rays {
		ray, ok := sample[want.RayID]
		if !ok {
			return &FogError{Code: E_INT, Message: "truth ray was not generated", RayID: want.RayID}
		}
		if err := checkRay(ray, want); err != nil {
			return err
		}
	}
	for _, want := range scene.Truth.Chunks {
		ray, _, _, err := CenterChunkRay(scene, want.ChunkX, want.ChunkY)
		if err != nil {
			return err
		}
		got, err := IntegrateRay(scene, ray, IntegrateOptions{
			Steps: scene.Recipe.Steps, ShadowSamples: scene.Recipe.ShadowSamples,
		})
		if err != nil {
			return err
		}
		dt := math.Abs(got.FinalTransmittance - want.Transmittance)
		if dt > tol.Transmittance {
			return &FogError{Code: E_INT, Message: "chunk center transmittance truth deviation",
				RayID: ray.ID, Chunk: &ChunkCoord{X: want.ChunkX, Y: want.ChunkY},
				Deviation: floatPtr(dt), Tolerance: floatPtr(tol.Transmittance)}
		}
		for c := 0; c < 3; c++ {
			dc := math.Abs(got.Color[c] - want.Color[c])
			if dc > tol.Color {
				return &FogError{Code: E_INT, Message: "chunk center color truth deviation",
					RayID: ray.ID, Chunk: &ChunkCoord{X: want.ChunkX, Y: want.ChunkY},
					Field: "color[" + itoa(c) + "]", Deviation: floatPtr(dc), Tolerance: floatPtr(tol.Color)}
			}
		}
	}
	return nil
}

func sameStrings(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}
