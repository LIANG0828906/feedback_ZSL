package renderer

import (
	"testing"

	"spatialir/internal/analyzer"
	"spatialir/internal/model"
)

func TestInterpolationResidual(t *testing.T) {
	if InterpolationResidual(0, 0) != 0 || InterpolationResidual(1, 0) != 0 {
		t.Fatal("integer delay must have zero interpolation residual")
	}
	if InterpolationResidual(0, 0.5) != 0.5 {
		t.Fatal("nearest residual mismatch")
	}
	if InterpolationResidual(1, 0.5) != 0.25 {
		t.Fatal("linear residual mismatch")
	}
	if InterpolationResidual(3, 0.5) >= InterpolationResidual(1, 0.5) {
		t.Fatal("cubic must be tighter than linear")
	}
}

func TestRenderFrameStaysInsideOwnedRange(t *testing.T) {
	n := 64
	ds := &analyzer.Dataset{
		Manifest: &model.Manifest{
			SampleRate: 48000, LengthSamples: n,
			Layout: model.ChannelLayout{Names: []string{"L"}, Channels: 1},
		},
		Signals: [][]float64{make([]float64, n)},
		Dry:     [][]float64{{1, 0, 0, 0}},
	}
	fr := analyzer.Frame{Index: 0, Start: 8, End: 24}
	est := []model.ChannelEstimate{{
		Channel: "L", DirectSample: 10, DirectEnergy: 1,
		Events: []model.Event{{Sample: 10, Amplitude: 1, Energy: 1, IsDirect: true}},
	}}
	p := model.FrameParams{ConvWindow: 64, RenderBlock: 8}
	out, err := RenderFrame(ds, fr, est, p, map[string]string{})
	if err != nil {
		t.Fatal(err)
	}
	for i := 0; i < n; i++ {
		if (i < fr.Start || i >= fr.End) && out[0][i] != 0 {
			t.Fatalf("write outside owned range at %d", i)
		}
	}
	merged := Merge([][][]float64{out}, 1, n)
	if len(merged[0]) != n {
		t.Fatal("merged length mismatch")
	}
}
