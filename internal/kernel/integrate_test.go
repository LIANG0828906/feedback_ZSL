package kernel_test

import (
	"math"
	"testing"

	"volumekernel/internal/generator"
	"volumekernel/internal/kernel"
	"volumekernel/internal/model"
	"volumekernel/internal/scene"
)

func TestRenderRayMatchesGeneratorTruth(t *testing.T) {
	s := generator.Generate(generator.DefaultSeed)
	if err := scene.Validate(s); err != nil {
		t.Fatal(err)
	}
	truth, err := generator.BuildTruth(s, []int{2})
	if err != nil {
		t.Fatal(err)
	}
	byRay := make(map[string]model.TruthRay, len(truth.Rays))
	for _, tr := range truth.Rays {
		byRay[tr.RayID] = tr
	}
	maxErr := 0.0
	compared := 0
	for y := 0; y < s.Viewport.Height; y++ {
		for x := 0; x < s.Viewport.Width; x++ {
			pr, _, err := kernel.RenderRay(s, s.CameraTrack.Samples[2], s.Viewport, x, y, kernel.Options{
				Steps:         s.Recipe.Steps,
				ShadowSamples: s.Recipe.ShadowSamples,
				StepBudget:    s.Recipe.Steps,
			})
			if err != nil {
				t.Fatalf("pixel %d,%d: %v", x, y, err)
			}
			tr := byRay[pr.RayID]
			d := math.Abs(pr.ScatteredRadiance - tr.ScatteredRadiance)
			if dt := math.Abs(pr.Transmittance - tr.Transmittance); dt > d {
				d = dt
			}
			if d > maxErr {
				maxErr = d
			}
			compared++
		}
	}
	t.Logf("compared=%d max_error=%.9f", compared, maxErr)
	if maxErr > s.Recipe.PixelTolerance {
		t.Fatalf("max error %.9f > tolerance %.9f", maxErr, s.Recipe.PixelTolerance)
	}
}
