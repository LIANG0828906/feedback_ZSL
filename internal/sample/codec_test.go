package sample

import "testing"

func TestQuantStepMonotone(t *testing.T) {
	prev := 0
	for _, q := range SupportedQualities {
		step := QuantStep(q)
		if step <= prev {
			t.Fatalf("quant step must grow as quality drops: q=%d step=%d", q, step)
		}
		prev = step
	}
}

func TestPixelBoundCoversObservedError(t *testing.T) {
	cfg := GenConfig{
		Seed: 42, Width: 32, Height: 32, Channels: 1, Frames: 2,
		BlockSize: 8, Qualities: SupportedQualities, SampleFrames: []int{0, 1},
		GeneratedAt: "test",
	}
	gen, err := Generate(cfg)
	if err != nil {
		t.Fatal(err)
	}
	for _, q := range gen.Manifest.QualityLevels {
		codec := NewCodec(8, q)
		bound := PixelBound(8, codec.Step)
		for f := range gen.Orig {
			_, reports := reconstruct(cfg, codec, gen.Orig[f])
			for _, rep := range reports {
				if rep.MaxAbsErr > bound {
					t.Fatalf("q=%d observed %d > bound %d", q, rep.MaxAbsErr, bound)
				}
			}
		}
	}
}

func TestManifestRejectsUnsupportedQuality(t *testing.T) {
	m := &Manifest{
		SchemaVersion: SchemaVersion, Width: 8, Height: 8, Channels: 1,
		Frames: 1, BlockSize: 8, QualityLevels: []int{33},
		SampleFrames: []int{0},
		Fingerprints: map[string]FrameFingerprint{
			"33:0": {Frame: 0, PSNRMin: 1, PSNRMax: 2, EdgePixels: []EdgePixel{{X: 0, Y: 0}},
				BlockQuantiles: []BlockQuantile{{BX: 0, BY: 0}}},
		},
	}
	if err := m.Validate(); err == nil {
		t.Fatal("unsupported quality must fail validation")
	}
}

func TestFrameRoundTripAndCorruption(t *testing.T) {
	f := &Frame{Width: 4, Height: 4, Channels: 1, Pix: []uint8{0, 1, 2, 3, 4, 5, 6, 7, 8, 9, 10, 11, 12, 13, 14, 15}}
	payload := EncodeFrame(f)
	got, err := DecodeFrame(payload)
	if err != nil {
		t.Fatal(err)
	}
	if len(got.Pix) != len(f.Pix) {
		t.Fatal("pixel length mismatch")
	}
	corrupt := append([]byte(nil), payload...)
	corrupt[len(corrupt)-1] ^= 0xFF
	if _, err := DecodeFrame(corrupt); err == nil {
		t.Fatal("corrupt CRC must be rejected")
	}
}
