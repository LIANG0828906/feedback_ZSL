package sample_test

import (
	"testing"

	"semanticshading/internal/colormetric"
	"semanticshading/internal/sample"
)

func TestSampleExpectationPointsMatchConstructedPixels(t *testing.T) {
	s, err := sample.Generate(sample.DefaultConfig())
	if err != nil {
		t.Fatal(err)
	}
	for _, expected := range s.Truth.Samples {
		offset := (expected.Y*s.Manifest.Width + expected.X) * s.Manifest.Channels
		got := colormetric.PixelDistanceQ8(s.Ref, s.Recon, s.Manifest.Channels, offset, s.Manifest.Metric)
		delta := got - expected.ExpectedDistanceQ8
		if delta < 0 {
			delta = -delta
		}
		if delta > s.Truth.Tolerance.SampleDistanceAbsQ8 {
			t.Fatalf("sample (%d,%d) = %d, truth %d, delta %d", expected.X, expected.Y, got, expected.ExpectedDistanceQ8, delta)
		}
	}
}
