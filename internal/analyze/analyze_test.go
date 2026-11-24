package analyze

import (
	"testing"

	"gameintegrity/internal/model"
)

func TestResidualAndNoiseSeparateRandomFrames(t *testing.T) {
	w, h := 32, 32
	base := make([]byte, w*h*3)
	for i := range base {
		base[i] = 40
	}
	random := make([]byte, len(base))
	for i := range random {
		random[i] = byte((i*73 + 19) & 0xff)
	}
	blocks := model.AllBlocks(w, h, 8)
	frame := model.FrameResult{Blocks: make([]model.BlockMetric, len(blocks))}
	metrics := Compute(Input{
		Frame: random, Previous: []Neighbor{{Index: 0, Pixels: base}},
		Width: w, Height: h, Channels: 3, BlockSize: 8,
		BlockMetric: frame.Blocks,
	})
	if metrics.ResidualEnergy < 0.05 {
		t.Fatalf("residual = %f, want a strong signal", metrics.ResidualEnergy)
	}
	if metrics.NoiseMean < 0.02 {
		t.Fatalf("noise mean = %f, want a strong signal", metrics.NoiseMean)
	}
}

func TestResidualThresholdMonotonic(t *testing.T) {
	blocks := make([]model.BlockMetric, 100)
	for i := range blocks {
		blocks[i].Residual = float64(i) / 100
	}
	previous := AbnormalBlockCountAt(blocks, 0)
	for _, threshold := range []float64{0.1, 0.2, 0.5, 0.9, 1.0} {
		got := AbnormalBlockCountAt(blocks, threshold)
		if got > previous {
			t.Fatalf("count increased from %d to %d at threshold %f", previous, got, threshold)
		}
		previous = got
	}
}

func TestBBoxShiftDetectsMovedShape(t *testing.T) {
	w, h := 32, 32
	expected := []model.ShapeBox{{ID: "r", X: 4, Y: 4, W: 8, H: 8, Color: [3]int{250, 10, 10}}}
	pixels := make([]byte, w*h*3)
	for y := 8; y < 16; y++ {
		for x := 8; x < 16; x++ {
			off := (y*w + x) * 3
			pixels[off], pixels[off+1], pixels[off+2] = 250, 10, 10
		}
	}
	shift := bboxShift(pixels, w, h, 3, expected)
	if shift < 4 {
		t.Fatalf("bbox shift = %f, want at least 4", shift)
	}
}
