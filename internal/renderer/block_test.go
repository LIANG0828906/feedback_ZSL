package renderer

import (
	"testing"

	"spatialir/internal/analyzer"
	"spatialir/internal/model"
)

func TestRenderBlockSizeIndependence(t *testing.T) {
	n := 128
	ds := &analyzer.Dataset{
		Manifest: &model.Manifest{
			SampleRate: 48000, LengthSamples: n,
			Layout: model.ChannelLayout{Names: []string{"L"}, Channels: 1},
		},
		Signals: [][]float64{make([]float64, n)},
		Dry:     [][]float64{{1, 0.5, 0.25}},
	}
	fr := analyzer.Frame{Index: 0, Start: 0, End: n}
	est := []model.ChannelEstimate{{
		Channel: "L", DirectSample: 10,
		Events: []model.Event{
			{Sample: 10, Amplitude: 1, Energy: 1, IsDirect: true},
			{Sample: 40, Amplitude: 0.5, Energy: 0.25},
		},
		TailOnset: 80, TailAmp: 0.2, TailRate: 0.01,
	}}
	base := model.FrameParams{ConvWindow: 3, InterpOrder: 1}
	var ref []float64
	for _, block := range []int{1, 7, 32, 128} {
		p := base
		p.RenderBlock = block
		out, err := RenderFrame(ds, fr, est, p, map[string]string{})
		if err != nil {
			t.Fatal(err)
		}
		if ref == nil {
			ref = append(ref, out[0]...)
			continue
		}
		for i := range ref {
			if ref[i] != out[0][i] {
				t.Fatalf("block %d changes sample %d: %v vs %v", block, i, ref[i], out[0][i])
			}
		}
	}
}
