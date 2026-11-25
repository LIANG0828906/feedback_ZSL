package fog

import (
	"errors"
	"testing"
)

func TestGenerateSampleDeterministicAndValid(t *testing.T) {
	first, err := GenerateSample()
	if err != nil {
		t.Fatal(err)
	}
	second, err := GenerateSample()
	if err != nil {
		t.Fatal(err)
	}
	if first.Scene.Media[0].ContentHash() != second.Scene.Media[0].ContentHash() {
		t.Fatal("medium content hash changed across fixed-seed generations")
	}
	if len(first.Truth.Rays) != first.Scene.Viewport.Width*first.Scene.Viewport.Height {
		t.Fatalf("truth ray count = %d", len(first.Truth.Rays))
	}
	if len(first.Truth.Tiles) != len(TileGrid(first.Scene.Viewport, first.Scene.Recipe)) {
		t.Fatalf("truth tile count = %d", len(first.Truth.Tiles))
	}
}

func TestSchemaValidationErrorFields(t *testing.T) {
	sample, err := GenerateSample()
	if err != nil {
		t.Fatal(err)
	}
	sample.Scene.Media[0].Absorption = -1
	err = ValidateScene(&sample.Scene)
	var fogErr *Error
	if !errors.As(err, &fogErr) || fogErr.Code != CodeSchema || fogErr.Field != "media[0].absorption" {
		t.Fatalf("expected E_SCHEMA absorption field, got %#v", err)
	}

	sample2, err := GenerateSample()
	if err != nil {
		t.Fatal(err)
	}
	sample2.Scene.Media[0].Bounds.Min.X = sample2.Scene.Media[0].Bounds.Max.X + FixedFromFloat(1)
	err = ValidateScene(&sample2.Scene)
	if fe := AsError(err); fe == nil || fe.Code != CodeSchema || fe.Field != "media[0].bounds" {
		t.Fatalf("expected bounds E_SCHEMA, got %#v", err)
	}

	sample3, err := GenerateSample()
	if err != nil {
		t.Fatal(err)
	}
	sample3.Scene.Recipe.MaxStepsPerRay = 0
	err = ValidateScene(&sample3.Scene)
	if fe := AsError(err); fe == nil || fe.Code != CodeSchema || fe.Field != "recipe.max_steps_per_ray" {
		t.Fatalf("expected recipe budget E_SCHEMA, got %#v", err)
	}
}

func TestCameraOrderInjectionIsEOrder(t *testing.T) {
	sample, _ := GenerateSample()
	sample.Scene.CameraTrack.Samples[1].LogicalTime = sample.Scene.CameraTrack.Samples[0].LogicalTime - 1
	err := ValidateScene(&sample.Scene)
	if fe := AsError(err); fe == nil || fe.Code != CodeOrder {
		t.Fatalf("expected E_ORDER, got %#v", err)
	}
}

func TestDecodeInjectionIsEDecodeWithMedium(t *testing.T) {
	sample, _ := GenerateSample()
	document, err := EncodeMedium(sample.Scene.Media[0])
	if err != nil {
		t.Fatal(err)
	}
	corrupt, mediumID, err := CorruptEncodedMedium(document)
	if err != nil {
		t.Fatal(err)
	}
	_, err = DecodeMedium(corrupt)
	fe := AsError(err)
	if fe == nil || fe.Code != CodeDecode || fe.MediumID != mediumID {
		t.Fatalf("expected E_DECODE for %s, got %#v", mediumID, err)
	}
}

func TestDensityInjectionIsEDensWithMedium(t *testing.T) {
	sample, _ := GenerateSample()
	graph := BuildDependencyGraph(&sample.Scene)
	_, err := PlanVoxelEdit(&sample.Scene, graph, "fog-a", [3]int{0, 0, 0}, MaxDensityVoxel+8)
	fe := AsError(err)
	if fe == nil || fe.Code != CodeDensity || fe.MediumID != "fog-a" {
		t.Fatalf("expected E_DENS fog-a, got %#v", err)
	}
}

func TestOverlapSegmentsAreOrderedAndQuantized(t *testing.T) {
	sample, _ := GenerateSample()
	ray := Ray{
		ID:     "ray-overlap-probe",
		Origin: Vec3{X: 0, Y: 1.45, Z: 6.4},
		Dir:    Vec3{X: 0, Y: -0.03125, Z: -0.999511}.Normalize(),
		Near:   FixedFromFloat(0.1),
		Far:    FixedFromFloat(40),
	}
	hits, err := IntersectRayMedia(ray, sample.Scene.Media)
	if err != nil {
		t.Fatal(err)
	}
	segments, err := BuildRaySegments(ray, hits)
	if err != nil {
		t.Fatal(err)
	}
	if len(segments) < 2 {
		t.Fatalf("expected multiple ordered segments, got %d", len(segments))
	}
	foundOverlap := false
	for i, segment := range segments {
		if len(segment.MediumIDs) >= 2 {
			foundOverlap = true
		}
		if i > 0 && segment.Near.Fixed < segments[i-1].Near.Fixed {
			t.Fatal("segments are not sorted")
		}
		if absFloat(segment.Near.Residual) > 1e-6 || absFloat(segment.Far.Residual) > 1e-6 {
			t.Fatalf("quantization residual too large: %+v", segment)
		}
	}
	if !foundOverlap {
		t.Fatal("expected an overlap segment containing both media")
	}
}

func TestUnorderableOverlapReturnsESegWithPair(t *testing.T) {
	rayID := "ray-bad-order"
	segments := []RaySegment{{
		Index: 0, MediumIDs: []string{"fog-a", "fog-b"}, PriorityKey: "fog-b+fog-a",
		Near: SegmentEndpoint{Fixed: FixedFromFloat(1)}, Far: SegmentEndpoint{Fixed: FixedFromFloat(2)},
	}}
	err := ValidateSegmentOrder(rayID, segments)
	fe := AsError(err)
	if fe == nil || fe.Code != CodeSeg || fe.RayID != rayID {
		t.Fatalf("expected E_SEG ray id, got %#v", err)
	}
	if fe.MediumPair[0] != "fog-a" || fe.MediumPair[1] != "fog-b" {
		t.Fatalf("expected medium pair, got %+v", fe.MediumPair)
	}
}

func TestTruthPrecisionAndMonotonicConvergence(t *testing.T) {
	sample, _ := GenerateSample()
	report := VerifyPrecision(sample, []int{8, 12, 18, 32, 48, 64})
	for i, point := range report.BudgetCurve {
		t.Logf("budget=%+v", point)
		if i > 0 && point.NormalizedDeviation > report.BudgetCurve[i-1].NormalizedDeviation+1e-9 {
			t.Fatalf("deviation increased at budget %d", point.StepBudget)
		}
	}
	if !report.Monotonic {
		t.Fatal("convergence curve is not monotonic")
	}
	if !report.TolerancePassed {
		t.Fatalf("full precision failed: %+v", report.FullBudget)
	}

	for _, rayTruth := range sample.Truth.Rays {
		if err := AssertRayTruthWithinTolerance(&sample.Scene, rayTruth, IntegrationOptions{}); err != nil {
			t.Fatalf("ray %s failed truth tolerance: %v", rayTruth.RayID, err)
		}
	}
}

func TestTightBudgetFailureIsEIterWithRayAndDeviation(t *testing.T) {
	sample, _ := GenerateSample()
	var found *Error
	for _, rayTruth := range sample.Truth.Rays {
		err := AssertRayTruthWithinTolerance(&sample.Scene, rayTruth, IntegrationOptions{StepBudget: 8, ShadowBudget: sample.Scene.Recipe.ShadowBudgetPerRay})
		if err != nil {
			found = AsError(err)
			break
		}
	}
	if found == nil || found.Code != CodeIter || found.RayID == "" || found.Deviation <= 0 {
		t.Fatalf("expected E_ITER with ray/deviation, got %#v", found)
	}
}

func absFloat(v float64) float64 {
	if v < 0 {
		return -v
	}
	return v
}
