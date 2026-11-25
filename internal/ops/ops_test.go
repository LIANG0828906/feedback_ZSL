package ops_test

import (
	"testing"

	"qualitybench/internal/core"
	"qualitybench/internal/model"
	"qualitybench/internal/ops"
)

func TestEveryStepPreservesGeometryAndPixelsOutsideRegion(t *testing.T) {
	src, err := core.NewFrame(64, 48, 3)
	if err != nil {
		t.Fatal(err)
	}
	for i := range src.Pix {
		src.Pix[i] = byte((i*37 + 19) % 251)
	}
	region := core.Rect{X: 14, Y: 9, W: 27, H: 21}
	steps := []model.Step{
		{Type: model.StepNoise, Strength: 0.4, Region: &region},
		{Type: model.StepBlur, Strength: 0.7, Region: &region},
		{Type: model.StepBlockiness, Strength: 0.5, Region: &region},
		{Type: model.StepChroma, Strength: -0.6, Region: &region},
		{Type: model.StepOverexpose, Strength: 0.5, Region: &region},
	}
	for _, step := range steps {
		dst, err := ops.ApplyStep(src, step, ops.Context{Seed: 12, StepIndex: 3})
		if err != nil {
			t.Fatalf("%s: %v", step.Type, err)
		}
		if dst.Width != src.Width || dst.Height != src.Height || dst.Channels != src.Channels {
			t.Fatalf("%s changed geometry", step.Type)
		}
		for y := 0; y < src.Height; y++ {
			for x := 0; x < src.Width; x++ {
				if region.Contains(x, y) {
					continue
				}
				if got, want := dst.At(x, y), src.At(x, y); got != want {
					t.Fatalf("%s changed outside pixel (%d,%d): %v != %v", step.Type, x, y, got, want)
				}
			}
		}
	}
}

func TestBlockApplicationUsesFullFrameContext(t *testing.T) {
	src, err := core.NewFrame(96, 64, 3)
	if err != nil {
		t.Fatal(err)
	}
	for y := 0; y < src.Height; y++ {
		for x := 0; x < src.Width; x++ {
			src.Set(x, y, [3]uint8{uint8(x * 2), uint8(y * 3), uint8(x + y)})
		}
	}
	step := model.Step{Type: model.StepBlur, Strength: 0.8, Region: &core.Rect{X: 8, Y: 8, W: 72, H: 48}}
	full, err := ops.ApplyStep(src, step, ops.Context{Seed: 1, StepIndex: 0})
	if err != nil {
		t.Fatal(err)
	}
	block := core.Rect{X: 16, Y: 16, W: 16, H: 16}
	got, err := ops.ApplyBlock(src, step, block, ops.Context{Seed: 1, StepIndex: 0})
	if err != nil {
		t.Fatal(err)
	}
	want, err := full.Block(block)
	if err != nil {
		t.Fatal(err)
	}
	if got.Hash() != want.Hash() {
		t.Fatalf("block application differs from full-frame context: %s != %s", got.Hash(), want.Hash())
	}
}
