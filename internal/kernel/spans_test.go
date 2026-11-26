package kernel_test

import (
	"math"
	"testing"

	"volumekernel/internal/camera"
	"volumekernel/internal/generator"
	"volumekernel/internal/kernel"
	"volumekernel/internal/model"
)

func TestOrderedSpansDoNotDependOnMediaTraversalOrder(t *testing.T) {
	s := generator.Generate(generator.DefaultSeed)
	sample := s.CameraTrack.Samples[2]
	ray, err := camera.RayForPixel(sample, s.Viewport, s.Viewport.Width/2, s.Viewport.Height/2)
	if err != nil {
		t.Fatal(err)
	}
	a, err := kernel.BuildOrderedSpans(s, ray)
	if err != nil {
		t.Fatal(err)
	}
	rev := generator.CloneScene(s)
	for i, j := 0, len(rev.Media)-1; i < j; i, j = i+1, j-1 {
		rev.Media[i], rev.Media[j] = rev.Media[j], rev.Media[i]
	}
	b, err := kernel.BuildOrderedSpans(rev, ray)
	if err != nil {
		t.Fatal(err)
	}
	if len(a) != len(b) {
		t.Fatalf("span count changed with traversal order: %d vs %d", len(a), len(b))
	}
	overlap := false
	for i := range a {
		if a[i].T0 != b[i].T0 || a[i].T1 != b[i].T1 || len(a[i].MediaIDs) != len(b[i].MediaIDs) {
			t.Fatalf("span %d changed: %+v vs %+v", i, a[i], b[i])
		}
		for j := range a[i].MediaIDs {
			if a[i].MediaIDs[j] != b[i].MediaIDs[j] {
				t.Fatalf("media order changed inside span: %v vs %v", a[i].MediaIDs, b[i].MediaIDs)
			}
		}
		if a[i].T0 < 0 || a[i].T1 <= a[i].T0 {
			t.Fatalf("invalid fixed-point span: %+v", a[i])
		}
		if a[i].T0ResidualTicks == 0 && a[i].T1ResidualTicks == 0 {
			// Residuals are allowed to be zero for tick-aligned geometry, but both
			// endpoints of every non-zero span must still be represented in ticks.
			if float64(a[i].T0)/float64(s.FixedScale) != float64(a[i].T0)/float64(s.FixedScale) {
				t.Fatal("unreachable")
			}
		}
		if len(a[i].MediaIDs) > 1 {
			overlap = true
		}
	}
	if !overlap {
		t.Fatal("test ray must contain an overlapping span")
	}
}

func TestStepAllocationIsExactAndDeterministic(t *testing.T) {
	spans := []kernel.OrderedSpan{
		{T0: 0, T1: 10},
		{T0: 10, T1: 30},
		{T0: 30, T1: 31},
	}
	for _, total := range []int{3, 4, 7, 16, 100} {
		got, err := kernel.AllocateSteps(spans, total)
		if err != nil {
			t.Fatal(err)
		}
		sum := 0
		for _, n := range got {
			if n < 1 {
				t.Fatalf("step allocation produced zero: %v", got)
			}
			sum += n
		}
		if sum != total {
			t.Fatalf("allocation sum=%d want %d: %v", sum, total, got)
		}
		again, _ := kernel.AllocateSteps(spans, total)
		for i := range got {
			if got[i] != again[i] {
				t.Fatalf("allocation is not deterministic: %v vs %v", got, again)
			}
		}
	}
	if _, err := kernel.AllocateSteps(spans, 2); err == nil {
		t.Fatal("expected budget error for fewer steps than spans")
	}
}

func TestErrorConvergenceIsMonotonic(t *testing.T) {
	s := generator.Generate(generator.DefaultSeed)
	truth, err := generator.BuildTruth(s, []int{2})
	if err != nil {
		t.Fatal(err)
	}
	byRay := make(map[string]model.TruthRay, len(truth.Rays))
	for _, tr := range truth.Rays {
		byRay[tr.RayID] = tr
	}
	stepsList := []int{16, 32, 64, 96, 192}
	last := math.Inf(1)
	for _, steps := range stepsList {
		maxErr := 0.0
		for y := 0; y < s.Viewport.Height; y++ {
			for x := 0; x < s.Viewport.Width; x++ {
				pr, _, err := kernel.RenderRay(s, s.CameraTrack.Samples[2], s.Viewport, x, y, kernel.Options{
					Steps:         steps,
					ShadowSamples: s.Recipe.ShadowSamples,
					StepBudget:    steps,
				})
				if err != nil {
					t.Fatal(err)
				}
				tr := byRay[pr.RayID]
				d := math.Abs(pr.ScatteredRadiance - tr.ScatteredRadiance)
				if dt := math.Abs(pr.Transmittance - tr.Transmittance); dt > d {
					d = dt
				}
				if d > maxErr {
					maxErr = d
				}
			}
		}
		t.Logf("steps=%d max_error=%.9f", steps, maxErr)
		if maxErr > last+1e-10 {
			t.Fatalf("error increased from %.9f to %.9f at %d steps", last, maxErr, steps)
		}
		last = maxErr
	}
}
