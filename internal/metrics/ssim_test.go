package metrics

import (
	"testing"

	"vqa/internal/fixed"
	"vqa/internal/sample"
)

func makePair(t *testing.T) (*sample.Frame, *sample.Frame) {
	t.Helper()
	cfg := sample.GenConfig{
		Seed: 5, Width: 32, Height: 32, Channels: 1, Frames: 1,
		BlockSize: 8, Qualities: []int{70}, SampleFrames: []int{0},
		GeneratedAt: "test",
	}
	gen, err := sample.Generate(cfg)
	if err != nil {
		t.Fatal(err)
	}
	codec := sample.NewCodec(8, 70)
	src := gen.Orig[0]
	rec := &sample.Frame{Width: src.Width, Height: src.Height, Channels: src.Channels, Pix: make([]uint8, len(src.Pix))}
	for by := 0; by < 4; by++ {
		for bx := 0; bx < 4; bx++ {
			tile := make([]uint8, 8*8)
			for y := 0; y < 8; y++ {
				for x := 0; x < 8; x++ {
					tile[y*8+x] = src.Pix[((by*8+y)*32 + (bx*8 + x))]
				}
			}
			br := codec.EncodeBlock(tile, 1)
			for y := 0; y < 8; y++ {
				for x := 0; x < 8; x++ {
					rec.Pix[((by*8+y)*32 + (bx*8 + x))] = br.Recon[y*8+x]
				}
			}
		}
	}
	return gen.Orig[0], rec
}

func TestBlockMergeEqualsFullFrameSSIM(t *testing.T) {
	orig, rec := makePair(t)
	p := DefaultParams(8, 32, 32, 1)
	tr := fixed.NewTracer()
	// Single-pass full-frame evaluation over the global grid.
	full := EvaluateGrid(orig, rec, p, tr)
	fullMean := MeanSSIM(full, tr)

	// Block path: each block emits only the windows it owns, then the frame
	// merge concatenates them in grid order.
	grid := BuildGrid(p)
	var merged []WindowResult
	for by := 0; by < 4; by++ {
		for bx := 0; bx < 4; bx++ {
			for _, w := range grid.Tops {
				if w.OwnerBX == bx && w.OwnerBY == by {
					s := EvaluateWindow(orig, rec, w.TopX, w.TopY, p, tr)
					w.SSIMR = int64(s)
					merged = append(merged, w)
				}
			}
		}
	}
	blockMean := MeanSSIM(merged, tr)
	if fullMean != blockMean {
		t.Fatalf("full-frame %d != block-merged %d", int64(fullMean), int64(blockMean))
	}
}

func TestTighterWindowYieldsFinerStats(t *testing.T) {
	orig, rec := makePair(t)
	base := DefaultParams(8, 32, 32, 1)
	tighter := base
	tighter.Window = 5
	tighter.Stride = 1
	tr := fixed.NewTracer()
	loose, _, _ := EvaluateBlockStats(orig, rec, 0, 0, base, tr)
	tight, _, _ := EvaluateBlockStats(orig, rec, 0, 0, tighter, tr)
	// Integer accumulators are independent of windowing: the block MSE must be
	// identical and the window count must grow monotonically as stride tightens.
	if loose.SumSqErr != tight.SumSqErr || loose.SampleCount != tight.SampleCount {
		t.Fatal("block integer accumulators must not depend on window parameters")
	}
	if len(BuildGrid(tighter).Tops) <= len(BuildGrid(base).Tops) {
		t.Fatal("tighter stride must increase the number of evaluated windows")
	}
}
