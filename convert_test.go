package textureconv

import (
	"context"
	"testing"
)

func TestQuantizationIdentityAndDirections(t *testing.T) {
	for _, d := range []BitDepth{Depth8, Depth16} {
		for v := 0; v <= int(d.Max()); v += 37 {
			for _, q := range []Quantization{QuantRound, QuantFloor, QuantCeil} {
				if got := Quantize(uint16(v), d, d, q); got != uint16(v) {
					t.Fatalf("same-depth %d %s changed %d -> %d", d, q, v, got)
				}
			}
		}
	}
	if got := Quantize(1, Depth8, Depth16, QuantFloor); got != 257 {
		t.Fatalf("8->16 exact expansion: got %d", got)
	}
	if got := Quantize(65535, Depth16, Depth8, QuantRound); got != 255 {
		t.Fatalf("16->8 round max: got %d", got)
	}
	if got := Quantize(1, Depth16, Depth8, QuantFloor); got != 0 {
		t.Fatalf("16->8 floor: got %d", got)
	}
	if got := Quantize(1, Depth16, Depth8, QuantCeil); got != 1 {
		t.Fatalf("16->8 ceil: got %d", got)
	}
	if got := Quantize(257, Depth16, Depth8, QuantRound); got != 1 {
		t.Fatalf("16->8 round: got %d", got)
	}
}

func TestConvertEntryPermutationFillDropAndAlpha(t *testing.T) {
	src := &StageImage{
		Width: 2, Height: 1, BitDepth: Depth16, Channels: 4, AlphaIndex: 3,
		Pixels: []uint16{
			65535, 0, 32768, 32768,
			0, 65535, 1000, 65535,
		},
	}
	e := Entry{
		Seq: 1, TargetDepth: Depth8, TargetChannels: 2,
		Permutation: []int{2, -1}, Fill: []FillSpec{{Channel: 1, Value: 7}},
		Drop: []int{0, 1, 3}, Quantization: QuantRound, Alpha: AlphaPreserve,
	}
	out, err := ConvertEntry(src, e)
	if err != nil {
		t.Fatal(err)
	}
	if out.Width != 2 || out.Height != 1 || out.Channels != 2 || out.BitDepth != Depth8 {
		t.Fatalf("unexpected output shape: %+v", out)
	}
	want0 := Quantize(32768, Depth16, Depth8, QuantRound)
	if out.Pixels[0] != want0 || out.Pixels[1] != 7 || out.Pixels[2] != Quantize(1000, Depth16, Depth8, QuantRound) || out.Pixels[3] != 7 {
		t.Fatalf("unexpected mapped pixels: %v", out.Pixels)
	}
	if len(out.Dropped) != 3 || out.Dropped[0] != (DroppedChannel{Entry: 1, Channel: 0}) {
		t.Fatalf("dropped channels not recorded: %+v", out.Dropped)
	}
	again, err := ConvertEntry(src, e)
	if err != nil {
		t.Fatal(err)
	}
	if HashStageImage(out) != HashStageImage(again) {
		t.Fatal("conversion is not deterministic")
	}
}

func TestAlphaModes(t *testing.T) {
	src := &StageImage{
		Width: 1, Height: 1, BitDepth: Depth8, Channels: 4, AlphaIndex: 3,
		Pixels: []uint16{100, 200, 50, 128},
	}
	base := Entry{Seq: 1, TargetDepth: Depth8, TargetChannels: 4,
		Permutation: []int{0, 1, 2, 3}, Fill: []FillSpec{}, Drop: []int{},
		Quantization: QuantRound, Alpha: AlphaPremultiply}
	out, err := ConvertEntry(src, base)
	if err != nil {
		t.Fatal(err)
	}
	if out.Pixels[0] != 50 || out.Pixels[1] != 100 || out.Pixels[2] != 25 || out.Pixels[3] != 128 {
		t.Fatalf("premultiply result: %v", out.Pixels)
	}
	base.Alpha = AlphaOpaque
	out, _ = ConvertEntry(src, base)
	if out.Pixels[3] != 255 {
		t.Fatalf("opaque alpha: %v", out.Pixels)
	}
	base.Alpha = AlphaStraighten
	premul := &StageImage{
		Width: 1, Height: 1, BitDepth: Depth8, Channels: 4, AlphaIndex: 3,
		Pixels: []uint16{50, 100, 25, 128},
	}
	out, err = ConvertEntry(premul, base)
	if err != nil {
		t.Fatal(err)
	}
	if out.Pixels[0] != 100 || out.Pixels[1] != 199 || out.Pixels[2] != 50 {
		t.Fatalf("straighten result: %v", out.Pixels)
	}
}

func TestRegionLeavesOutsidePixelsUnchanged(t *testing.T) {
	im := testImage(t, 40, 30, Depth8, LayoutRGBA)
	src := &StageImage{Width: im.Width, Height: im.Height, BitDepth: im.BitDepth, Channels: 4, AlphaIndex: 3, Pixels: append([]uint16(nil), im.Pixels...)}
	region := Rect{X: 10, Y: 8, W: 12, H: 9}
	e := Entry{Seq: 1, TargetDepth: Depth8, TargetChannels: 4,
		Permutation: []int{2, 1, 0, 3}, Fill: []FillSpec{}, Drop: []int{},
		Quantization: QuantRound, Alpha: AlphaOpaque, Region: &region}
	out, err := ConvertEntry(src, e)
	if err != nil {
		t.Fatal(err)
	}
	for y := 0; y < im.Height; y++ {
		for x := 0; x < im.Width; x++ {
			off := (y*im.Width + x) * 4
			if region.Contains(x, y) {
				if out.Pixels[off+3] != 255 {
					t.Fatalf("inside alpha not opaque at %d,%d", x, y)
				}
			} else {
				for c := 0; c < 4; c++ {
					if out.Pixels[off+c] != src.Pixels[off+c] {
						t.Fatalf("outside pixel changed at %d,%d c%d", x, y, c)
					}
				}
			}
		}
	}
}

func TestGeneratedTruthAgainstConverter(t *testing.T) {
	b := testBundle(t, SamplePlan())
	for _, rec := range b.Manifest.Images {
		it, _ := b.ImageTruth(rec.ID)
		stage := &StageImage{
			Width: rec.Width, Height: rec.Height, BitDepth: rec.BitDepth,
			Channels: 4, AlphaIndex: 3, Pixels: append([]uint16(nil), b.Images[rec.ID].Pixels...),
		}
		for _, e := range b.Plan.Entries {
			out, err := ConvertEntry(stage, e)
			if err != nil {
				t.Fatalf("%s entry %d: %v", rec.ID, e.Seq, err)
			}
			stage = out
			for _, s := range it.Samples {
				if s.Entry != e.Seq {
					continue
				}
				got := stage.Pixel(s.X, s.Y)
				if stage.BitDepth != s.BitDepth {
					t.Fatalf("%s entry %d depth %d != truth %d", rec.ID, e.Seq, stage.BitDepth, s.BitDepth)
				}
				for c := range s.Channels {
					if absDiff(got[c], s.Channels[c]) > s.Tolerance {
						t.Fatalf("%s entry %d pixel %d,%d channel %d got %d want %d tol %d",
							rec.ID, e.Seq, s.X, s.Y, c, got[c], s.Channels[c], s.Tolerance)
					}
				}
			}
		}
	}
}

func TestEngineParallelEqualsSerialAndTruth(t *testing.T) {
	b := testBundle(t, SamplePlan())
	for _, rec := range b.Manifest.Images {
		dir := t.TempDir()
		var snaps []*RunSnapshot
		for i, workers := range []int{1, 4} {
			store, _ := openTestStore(t, dir, "db"+string(rune('a'+i))+".db")
			e, err := NewEngine(b.Images[rec.ID], b.Plan, EngineOptions{
				ImageID: rec.ID, SourceHash: rec.SHA256, ParamsVersion: "test-v1",
				TileSize: 24, Workers: workers, Store: store,
			})
			if err != nil {
				store.Close()
				t.Fatalf("NewEngine: %v", err)
			}
			processAll(t, e)
			snap, err := e.Snapshot()
			if err != nil {
				store.Close()
				t.Fatalf("Snapshot: %v", err)
			}
			if _, err := VerifyPersisted(b, rec.ID, store, VerifyOptions{
				ExpectedParamsVersion: "test-v1", ExpectedAlgorithmVersion: AlgorithmVersion,
			}); err != nil {
				store.Close()
				t.Fatalf("VerifyPersisted: %v", err)
			}
			snaps = append(snaps, snap)
			store.Close()
		}
		if err := CompareSnapshots(snaps[0], snaps[1]); err != nil {
			t.Fatalf("parallel vs serial: %v", err)
		}
	}
}

func TestProcessLimitPreservesCommitted(t *testing.T) {
	b := testBundle(t, SamplePlan())
	rec := b.Manifest.Images[0]
	store, _ := openTestStore(t, t.TempDir(), "cancel.db")
	defer store.Close()
	e, err := NewEngine(b.Images[rec.ID], b.Plan, EngineOptions{
		ImageID: rec.ID, SourceHash: rec.SHA256, ParamsVersion: "test-v1",
		TileSize: 24, Workers: 2, Store: store,
	})
	if err != nil {
		t.Fatal(err)
	}
	run, err := e.ProcessLimit(context.Background(), 3)
	if err != nil {
		t.Fatalf("partial ProcessLimit: %v", err)
	}
	if run.Progress.CommittedTiles == 0 || run.Progress.CommittedTiles >= run.Progress.TotalTiles {
		t.Fatalf("expected partial progress, got %d/%d", run.Progress.CommittedTiles, run.Progress.TotalTiles)
	}
}
