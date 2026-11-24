package pipeline_test

import (
	"context"
	"errors"
	"math"
	"testing"

	"virtualstage/pipeline"
)

func TestCompositePremultipliedMatchesStraight(t *testing.T) {
	straight := testModel()
	premultiplied := testModel()
	premultiplied.Layers[0].AlphaMode = pipeline.AlphaPremultiplied
	premultiplied.Layers[0].Color = [3]float64{0.3 * 0.5, 0.2 * 0.5, 0.1 * 0.5}
	straight.Layers[0].Color = [3]float64{0.3, 0.2, 0.1}
	straight.Layers[0].Alpha = 0.5
	premultiplied.Layers[0].Alpha = 0.5

	a, err := pipeline.CompositeFrame(straight, 0, pipeline.CompositeConfig{MaxFrameBytes: 1 << 20})
	if err != nil {
		t.Fatal(err)
	}
	b, err := pipeline.CompositeFrame(premultiplied, 0, pipeline.CompositeConfig{MaxFrameBytes: 1 << 20})
	if err != nil {
		t.Fatal(err)
	}
	for i := range a.Pixels {
		if math.Abs(float64(a.Pixels[i]-b.Pixels[i])) > 1e-6 {
			t.Fatalf("pixel channel %d differs: %g vs %g", i, a.Pixels[i], b.Pixels[i])
		}
	}
}

func TestCompositeLinearizesSRGBAndIsDeterministic(t *testing.T) {
	model := testModel()
	model.Layers[0].Color = [3]float64{0.5, 0.25, 0.1}
	model.Layers[0].ColorEncoding = pipeline.ColorSRGB
	model.Layers[0].Alpha = 1
	frame, err := pipeline.CompositeFrame(model, 0, pipeline.CompositeConfig{MaxFrameBytes: 1 << 20})
	if err != nil {
		t.Fatal(err)
	}
	sample, _ := frame.Sample(0, 0)
	wantR := math.Pow((0.5+0.055)/1.055, 2.4)
	if math.Abs(sample.RGBA[0]-wantR) > 1e-6 {
		t.Fatalf("sRGB linearization = %g, want %g", sample.RGBA[0], wantR)
	}
	second, err := pipeline.CompositeFrame(model, 0, pipeline.CompositeConfig{MaxFrameBytes: 1 << 20})
	if err != nil {
		t.Fatal(err)
	}
	if pipeline.FrameHash(frame) != pipeline.FrameHash(second) {
		t.Fatal("frame hash is not deterministic")
	}
	if frame.Timestamp != pipeline.NewRational(0, 1) {
		t.Fatalf("timestamp = %v", frame.Timestamp)
	}
}

func TestCompositeErrors(t *testing.T) {
	model := testModel()
	model.Layers[0].Opacity = 1.2
	if err := model.Validate(); err == nil {
		t.Fatal("expected parameter error")
	} else {
		assertCode(t, err, pipeline.CodeParameter)
	}

	model = testModel()
	model.Stack = []string{"missing"}
	if err := model.Validate(); err == nil {
		t.Fatal("expected layer error")
	} else {
		assertCode(t, err, pipeline.CodeLayer)
	}

	model = testModel()
	_, err := pipeline.CompositeFrame(model, 0, pipeline.CompositeConfig{MaxFrameBytes: 4})
	assertCode(t, err, pipeline.CodeParameter)
}

func TestCompareFindsFirstMismatchPixel(t *testing.T) {
	model := testModel()
	a, err := pipeline.CompositeFrame(model, 0, pipeline.CompositeConfig{MaxFrameBytes: 1 << 20})
	if err != nil {
		t.Fatal(err)
	}
	b, _ := pipeline.CompositeFrame(model, 0, pipeline.CompositeConfig{MaxFrameBytes: 1 << 20})
	b.Pixels[(0*2+1)*4+2] += 0.25
	mismatch := pipeline.FirstFrameMismatch(3, a, b)
	if !mismatch.Found || mismatch.Window != 3 || mismatch.X != 1 || mismatch.Y != 0 || mismatch.Channel != 2 {
		t.Fatalf("mismatch = %+v", mismatch)
	}
}

func TestWindowStateMachine(t *testing.T) {
	windows, err := pipeline.NewWindowSet(12, 6)
	if err != nil {
		t.Fatal(err)
	}
	if err := windows.Transition(0, pipeline.WindowCommitted); err == nil {
		t.Fatal("expected illegal transition")
	} else {
		assertCode(t, err, pipeline.CodeState)
	}
	window, _ := windows.Get(0)
	if window.State != pipeline.WindowPending {
		t.Fatalf("state changed after illegal transition: %s", window.State)
	}
	if _, err := windows.Begin(0); err != nil {
		t.Fatal(err)
	}
	snapshot := pipeline.WindowSnapshot{
		WindowIndex: 0, StartFrame: 0, EndFrame: 6,
		AlgorithmVersion: pipeline.AlgorithmVersion,
		SourceHash:       "source", ModelHash: "model", FrameHash: "frame",
	}
	if _, err := windows.Commit(0, snapshot, 1); err != nil {
		t.Fatal(err)
	}
	snapshot.WindowIndex = 99
	window, _ = windows.Get(0)
	if window.Snapshot.WindowIndex != 0 {
		t.Fatal("committed snapshot was not locked against caller mutation")
	}
	committed, total := windows.Progress()
	if committed != 1 || total != 2 {
		t.Fatalf("progress = %d/%d", committed, total)
	}
	if err := windows.Fail(0, errors.New("late failure")); err == nil {
		t.Fatal("committed window accepted failure")
	}
	if err := windows.Invalidate(0, pipeline.Invalidation{Reason: "test"}); err != nil {
		t.Fatal(err)
	}
	window, _ = windows.Get(0)
	if window.State != pipeline.WindowPending || window.Snapshot != nil || !window.Invalidated {
		t.Fatalf("invalidation = %+v", window)
	}
}

func TestCompositeWindowUsesOneAtomicPacket(t *testing.T) {
	model := testModel()
	model.FrameCount = 3
	window := pipeline.Window{Index: 0, StartFrame: 0, EndFrame: 3}
	result, err := pipeline.CompositeWindow(context.Background(), model, window, pipeline.CompositeConfig{MaxFrameBytes: 1 << 20})
	if err != nil {
		t.Fatal(err)
	}
	if len(result.Frames) != 3 || len(result.FrameSummaries) != 3 || result.ContentHash == "" {
		t.Fatalf("result = %+v", result)
	}
	decoded, err := pipeline.DecodeWindow(pipeline.EncodeWindow(result))
	if err != nil {
		t.Fatal(err)
	}
	if decoded.ContentHash != result.ContentHash {
		t.Fatal("window packet hash changed after round trip")
	}
}

func testModel() *pipeline.Model {
	return &pipeline.Model{
		Name: "test", Canvas: pipeline.Canvas{Width: 2, Height: 1, Channels: 4, ColorSpace: pipeline.ColorLinear},
		TimeBase:   pipeline.TimeBase{Unit: "second", FramePeriod: pipeline.NewRational(1, 24)},
		FrameCount: 1,
		Layers: []pipeline.Layer{{
			ID: "foreground", Name: "Foreground", Kind: "prop", Visible: true,
			InFrame: 0, OutFrame: 1, VisibleUntilFrame: 1, Opacity: 1, Alpha: 0.5,
			Color: [3]float64{0.4, 0.3, 0.2}, ColorEncoding: pipeline.ColorLinear,
			AlphaMode: pipeline.AlphaStraight, Shape: pipeline.Shape{Type: pipeline.ShapeFull},
			SourceVersion: 1,
		}},
		Stack: []string{"foreground"}, SourceVersions: []int{1}, SourceVersion: 1,
	}
}
