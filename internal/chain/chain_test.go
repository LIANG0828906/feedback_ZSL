package chain_test

import (
	"errors"
	"testing"

	"qkernel/internal/chain"
	"qkernel/internal/errs"
	"qkernel/internal/imagex"
	"qkernel/internal/synth"
)

func TestValidationErrorsPointToStepAndField(t *testing.T) {
	err := chain.Validate(chain.Chain{}, 128, 128)
	if !errs.IsCode(err, errs.EPara) {
		t.Fatalf("empty chain expected E_PARA, got %v", err)
	}

	c := chain.Chain{Steps: []chain.Step{{Type: "magic", Strength: 0.5, Region: chain.WholeRegion()}}}
	err = chain.Validate(c, 128, 128)
	ke := asError(t, err)
	if ke.Code != errs.EPara || ke.StepIndex != 1 || ke.Field != "steps[0].type" {
		t.Fatalf("unexpected unknown-type error: %+v", ke)
	}

	c = chain.Chain{Steps: []chain.Step{{Type: chain.NoiseStep, Strength: 1.5, Region: chain.WholeRegion()}}}
	err = chain.Validate(c, 128, 128)
	ke = asError(t, err)
	if ke.StepIndex != 1 || ke.Field != "steps[0].strength" {
		t.Fatalf("unexpected strength error: %+v", ke)
	}
}

func TestStepsPreserveOutsideRegionBytes(t *testing.T) {
	b, err := synth.Generate(synth.Config{Width: 128, Height: 128, Seed: 9, Channels: 3}, 0)
	if err != nil {
		t.Fatal(err)
	}
	region := imagex.R(18, 22, 74, 81)
	types := []chain.StepType{chain.NoiseStep, chain.BlurStep, chain.BlockStep, chain.ChromaStep, chain.OverexposeStep}
	for i, typ := range types {
		step := chain.Step{Type: typ, Strength: 0.7, Region: chain.RectRegion(region)}
		out := chain.ApplyStep(b.Frame, step, i, 1234)
		if out.Width != b.Frame.Width || out.Height != b.Frame.Height || out.Channels != b.Frame.Channels {
			t.Fatalf("%s changed geometry", typ)
		}
		for y := 0; y < b.Frame.Height; y++ {
			for x := 0; x < b.Frame.Width; x++ {
				if region.Contains(x, y) {
					continue
				}
				oi := b.Frame.Offset(x, y)
				for ch := 0; ch < b.Frame.Channels; ch++ {
					if out.Pix[oi+ch] != b.Frame.Pix[oi+ch] {
						t.Fatalf("%s modified outside ROI at (%d,%d) ch%d", typ, x, y, ch)
					}
				}
			}
		}
	}
}

func asError(t *testing.T, err error) *errs.Error {
	t.Helper()
	var ke *errs.Error
	if !errors.As(err, &ke) {
		t.Fatalf("expected *errs.Error, got %T %v", err, err)
	}
	return ke
}
