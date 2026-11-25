package pipeline_test

import (
	"errors"
	"math"
	"reflect"
	"testing"

	"ms-workbench/internal/apperr"
	"ms-workbench/internal/pipeline"
	"ms-workbench/internal/samples"
	"ms-workbench/internal/scene"
)

func testScene(t *testing.T) *scene.Scene {
	t.Helper()
	pkg, err := samples.Generate(samples.Options{Seed: 20261125, Width: 64, Height: 48}, "pipeline", "Pipeline Test")
	if err != nil {
		t.Fatal(err)
	}
	s, err := scene.Decode(pkg.Files[3])
	if err != nil {
		t.Fatal(err)
	}
	return s
}

func TestPatchAccuracyPeakAndGamutObservability(t *testing.T) {
	s := testScene(t)
	rect := scene.Rect{X: 0, Y: 0, W: s.Width, H: s.Height}
	result, err := pipeline.ProcessChunk(s, rect, s.Settings)
	if err != nil {
		t.Fatal(err)
	}
	if math.Abs(result.Fused.PeakRadiance-s.Truth.PeakRadiance) > s.Truth.PatchTolerance {
		t.Fatalf("peak radiance %.8f, truth %.8f", result.Fused.PeakRadiance, s.Truth.PeakRadiance)
	}
	measurements := pipeline.MeasurePatches(s, result.Chroma.LinearRGB)
	for _, m := range measurements {
		if !m.Pass {
			t.Fatalf("%s delta %.8f exceeds tolerance %.8f (expected=%v actual=%v)",
				m.Name, m.Delta, s.Truth.PatchTolerance, m.Expected, m.Actual)
		}
	}
	if len(result.Chroma.OutOfGamut) == 0 {
		t.Fatal("expected the synthetic spectral magenta patch to be observable as out of gamut")
	}
	if len(result.Chroma.Boundary) != len(result.Chroma.OutOfGamut) {
		t.Fatalf("boundary count %d != out-of-gamut count %d", len(result.Chroma.Boundary), len(result.Chroma.OutOfGamut))
	}
	for _, b := range result.Chroma.Boundary {
		if b.MappedRGB[0] < 0 || b.MappedRGB[1] < 0 || b.MappedRGB[2] < 0 ||
			b.MappedRGB[0] > 1 || b.MappedRGB[1] > 1 || b.MappedRGB[2] > 1 {
			t.Fatalf("gamut compression left a coordinate outside the unit cube: %+v", b)
		}
	}
}

func TestDeterministicInterpolationOrders(t *testing.T) {
	s := testScene(t)
	rect := scene.Rect{X: 5, Y: 7, W: 23, H: 17}
	for _, order := range []int{0, 1, 2, 3, 4, 5} {
		settings := s.Settings
		settings.InterpolationOrder = order
		a, err := pipeline.ProcessChunk(s, rect, settings)
		if err != nil {
			t.Fatal(err)
		}
		b, err := pipeline.ProcessChunk(s, rect, settings)
		if err != nil {
			t.Fatal(err)
		}
		if !reflect.DeepEqual(a, b) {
			t.Fatalf("interpolation order %d is not deterministic", order)
		}
	}
}

func TestPatchTruthTracksDeclaredGamutIntent(t *testing.T) {
	s := testScene(t)
	rect := scene.Rect{X: 0, Y: 0, W: s.Width, H: s.Height}
	for _, intent := range []string{"relative-colorimetric", "perceptual", "saturation"} {
		settings := s.Settings
		settings.GamutIntent = intent
		s.Settings = settings
		result, err := pipeline.ProcessChunk(s, rect, settings)
		if err != nil {
			t.Fatal(err)
		}
		for _, m := range pipeline.MeasurePatches(s, result.Chroma.LinearRGB) {
			if !m.Pass {
				t.Fatalf("intent %s patch %s delta %.8f", intent, m.Name, m.Delta)
			}
		}
	}
}

func TestRegistrationRangeErrorContainsPairAndOffset(t *testing.T) {
	s := testScene(t)
	s.Bands[2].OffsetX = 9
	_, err := pipeline.ProcessChunk(s, scene.Rect{W: 4, H: 4}, s.Settings)
	if !apperr.IsCode(err, apperr.EREG) {
		t.Fatalf("error = %v", err)
	}
	var ae *apperr.Error
	if !errors.As(err, &ae) {
		t.Fatal("not an app error")
	}
	if ae.BandPair == "" || ae.Measured < 9 || ae.OffsetX != 9 || ae.Limit != s.Settings.MaxRegistrationOffset {
		t.Fatalf("missing registration context: %+v", ae)
	}
}
