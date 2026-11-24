package watermark_test

import (
	"image"
	"testing"

	"integritykernel/internal/core"
	wm "integritykernel/internal/watermark"
)

func TestDecodeDeterministicAndRepeats(t *testing.T) {
	params := core.DefaultParams()
	region := core.Rect{X: 20, Y: 12, Width: 80, Height: 24}
	img := image.NewNRGBA(image.Rect(0, 0, 160, 90))
	for frame := 0; frame < 18; frame++ {
		wm.Draw(img, frame, region, params)
		first, err := wm.Decode(img, frame, region, params)
		if err != nil {
			t.Fatal(err)
		}
		for repeat := 0; repeat < 20; repeat++ {
			next, err := wm.Decode(img, frame, region, params)
			if err != nil {
				t.Fatal(err)
			}
			if core.MustJSON(first) != core.MustJSON(next) {
				t.Fatalf("non-deterministic decode for frame %d", frame)
			}
		}
		if first.DecodedFrame != frame {
			t.Fatalf("frame %d decoded %d", frame, first.DecodedFrame)
		}
	}
}

func TestMissingAndOutOfBoundsMark(t *testing.T) {
	params := core.DefaultParams()
	region := core.Rect{X: 20, Y: 12, Width: 80, Height: 24}
	blank := image.NewNRGBA(image.Rect(0, 0, 160, 90))
	evidence, err := wm.Decode(blank, 3, region, params)
	if err != nil {
		t.Fatal(err)
	}
	if !evidence.Missing {
		t.Fatalf("expected missing watermark, got confidence %.3f", evidence.OverallConfidence)
	}

	bad := core.Rect{X: 150, Y: 10, Width: 80, Height: 24}
	_, err = wm.Decode(blank, 0, bad, params)
	ke, ok := core.AsError(err)
	if !ok || ke.Code != core.CodeMark || ke.Field != "watermark_region" {
		t.Fatalf("expected E_MARK watermark_region, got %#v", err)
	}
}
