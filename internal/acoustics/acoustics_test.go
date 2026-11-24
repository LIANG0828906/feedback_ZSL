package acoustics

import (
	"math"
	"reflect"
	"sync"
	"testing"

	"spatialworkbench/internal/generator"
	"spatialworkbench/internal/model"
	"spatialworkbench/internal/werr"
)

func testSample() model.Sample {
	return generator.Generate(generator.Options{Seed: 99, Samples: 1, Prefix: "acoustics"}).Samples[0]
}

func TestParamsValidationFields(t *testing.T) {
	sample := testSample()
	params := model.DefaultParams(sample)
	cases := []struct {
		mutate func(*model.Params)
		field  string
	}{
		{func(p *model.Params) { p.FrameLength = 0 }, "frameLength"},
		{func(p *model.Params) { p.Hop = 0 }, "hop"},
		{func(p *model.Params) { p.Hop = p.FrameLength + 1 }, "hop"},
		{func(p *model.Params) { p.WindowLength = 0 }, "windowLength"},
		{func(p *model.Params) { p.WindowLength = p.FrameLength + 1 }, "windowLength"},
	}
	for _, tc := range cases {
		p := params.Clone()
		tc.mutate(&p)
		err := p.Validate(sample)
		if err == nil || err.Code != werr.CodePara || err.Field != tc.field {
			t.Fatalf("field %s: got %#v", tc.field, err)
		}
	}
}

func TestEstimateQuantizedAndDeterministic(t *testing.T) {
	sample := testSample()
	params := model.DefaultParams(sample)
	total := params.TotalFrames(len(sample.Dry.Samples))
	baseline := make([]FrameEstimate, total)
	for i := 0; i < total; i++ {
		estimate, err := EstimateFrame(sample, params, i)
		if err != nil {
			t.Fatalf("frame %d: %v", i, err)
		}
		baseline[i] = estimate
		for _, ch := range estimate.Channels {
			if ch.DirectArrival%params.QuantStep != 0 {
				t.Fatalf("direct arrival %d is not a multiple of %d", ch.DirectArrival, params.QuantStep)
			}
			if ch.IntegratedWidth > params.WindowLength {
				t.Fatalf("integrated window %d exceeds %d", ch.IntegratedWidth, params.WindowLength)
			}
			if ch.DirectArrival+1+ch.IntegratedWidth > estimate.End {
				t.Fatalf("energy window crossed frame boundary")
			}
		}
	}
	var wg sync.WaitGroup
	for i := 0; i < total; i++ {
		i := i
		for worker := 0; worker < 8; worker++ {
			wg.Add(1)
			go func() {
				defer wg.Done()
				got, err := EstimateFrame(sample, params, i)
				if err != nil || !reflect.DeepEqual(got, baseline[i]) {
					t.Errorf("frame %d differed by completion order: %v", i, err)
				}
			}()
		}
	}
	wg.Wait()
}

func TestReconstructRenderLengthAndRepeatability(t *testing.T) {
	sample := testSample()
	params := model.DefaultParams(sample)
	total := params.TotalFrames(len(sample.Dry.Samples))
	previous := ReconstructionState{}
	rendered := 0
	for i := 0; i < total; i++ {
		estimate, err := EstimateFrame(sample, params, i)
		if err != nil {
			t.Fatal(err)
		}
		recon, err := Reconstruct(sample, params, estimate, previous)
		if err != nil {
			t.Fatal(err)
		}
		left, err := RenderFrame(sample, params, estimate, recon)
		if err != nil {
			t.Fatal(err)
		}
		right, err := RenderFrame(sample, params, estimate, recon)
		if err != nil {
			t.Fatal(err)
		}
		if !reflect.DeepEqual(left, right) {
			t.Fatalf("frame %d render is not repeatable", i)
		}
		expected := params.Hop
		if remaining := len(sample.Dry.Samples) - i*params.Hop; remaining < expected {
			expected = remaining
		}
		for ch := range left {
			if len(left[ch]) != expected {
				t.Fatalf("frame %d channel %d length %d, want %d", i, ch, len(left[ch]), expected)
			}
			for _, v := range left[ch] {
				if math.IsNaN(v) || math.IsInf(v, 0) {
					t.Fatalf("non-finite render")
				}
			}
			rendered += len(left[ch])
		}
		previous = recon.State
	}
	if got := rendered / len(sample.Layout.Channels); got != len(sample.Dry.Samples) {
		t.Fatalf("total per-channel render %d, dry %d", got, len(sample.Dry.Samples))
	}
}

func TestLayoutAndToleranceErrors(t *testing.T) {
	sample := testSample()
	params := model.DefaultParams(sample)
	params.Routing[sample.Channels[0].ID] = 99
	err := params.Validate(sample)
	if err == nil || err.Code != werr.CodeLayout || err.Channel == "" {
		t.Fatalf("expected E_LAYOUT with channel, got %#v", err)
	}
	params = model.DefaultParams(sample)
	params.ErrorTolerance = 0.01
	estimate, estErr := EstimateFrame(sample, params, 0)
	if estErr != nil {
		t.Fatal(estErr)
	}
	_, reconErr := Reconstruct(sample, params, estimate, ReconstructionState{})
	if reconErr == nil || reconErr.Code != werr.CodeErr || reconErr.Frame != 0 || reconErr.Item == "" {
		t.Fatalf("expected E_ERR with frame and item, got %#v", reconErr)
	}
	params = model.DefaultParams(sample)
	params.ErrorTolerance = 9
	previous := ReconstructionState{}
	for frame := 0; frame < 2; frame++ {
		estimate, estErr := EstimateFrame(sample, params, frame)
		if estErr != nil {
			t.Fatal(estErr)
		}
		recon, reconErr := Reconstruct(sample, params, estimate, previous)
		if frame == 0 {
			if reconErr != nil {
				t.Fatalf("frame 0 should fit tolerance 9: %v", reconErr)
			}
			previous = recon.State
			continue
		}
		if reconErr == nil || reconErr.Code != werr.CodeErr || reconErr.Frame != 1 || reconErr.Item != ErrorITD {
			t.Fatalf("expected cumulative E_ERR at frame 1, got %#v", reconErr)
		}
	}
}

func TestRenderRejectsReconstructedOverflow(t *testing.T) {
	sample := testSample()
	params := model.DefaultParams(sample)
	estimate, err := EstimateFrame(sample, params, 0)
	if err != nil {
		t.Fatal(err)
	}
	recon, err := Reconstruct(sample, params, estimate, ReconstructionState{})
	if err != nil {
		t.Fatal(err)
	}
	recon.Channels[0].DirectEnergy = 4
	_, err = RenderFrame(sample, params, estimate, recon)
	if err == nil || err.Code != werr.CodeClip || err.Frame != 0 || err.Channel == "" {
		t.Fatalf("expected E_CLIP with frame/channel, got %#v", err)
	}
}
