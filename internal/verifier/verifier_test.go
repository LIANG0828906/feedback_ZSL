package verifier

import (
	"context"
	"path/filepath"
	"testing"

	"vqa/internal/metrics"
	"vqa/internal/pipeline"
	"vqa/internal/sample"
	"vqa/internal/store"
)

func buildStore(t *testing.T, quality int) (*sample.Manifest, *store.Store) {
	t.Helper()
	base := t.TempDir()
	data := filepath.Join(base, "data")
	cfg := sample.GenConfig{
		Seed: 11, Width: 32, Height: 32, Channels: 1, Frames: 3,
		BlockSize: 8, Qualities: []int{90, 70, 50}, SampleFrames: []int{0, 2},
		GeneratedAt: "test",
	}
	gen, err := sample.Generate(cfg)
	if err != nil {
		t.Fatal(err)
	}
	if err := sample.WritePersisted(data, gen); err != nil {
		t.Fatal(err)
	}
	seq, err := sample.LoadSequence(data, quality)
	if err != nil {
		t.Fatal(err)
	}
	params := metrics.DefaultParams(8, 32, 32, 1)
	eng := &pipeline.Engine{
		Seq: seq, Params: params, StoreDir: filepath.Join(base, "store"),
		Workers: 2, MaxPixelBuf: 4096, EdgeTol: 12,
	}
	st, err := eng.Run(context.Background(), pipeline.Rebuild)
	if err != nil {
		t.Fatal(err)
	}
	return gen.Manifest, st
}

func TestVerifyPassesOnValidStore(t *testing.T) {
	for _, q := range []int{90, 70, 50} {
		manifest, st := buildStore(t, q)
		rep, err := Verify(manifest, st, q)
		if err != nil {
			t.Fatalf("quality %d verify: %v", q, err)
		}
		if rep.BlocksChecked == 0 || rep.FramesChecked != manifest.Frames {
			t.Fatalf("quality %d unexpected coverage %+v", q, rep)
		}
	}
}

func TestPSNRDegradesAsStepGrows(t *testing.T) {
	var last float64
	for i, q := range []int{90, 70, 50} {
		manifest, st := buildStore(t, q)
		rep, err := Verify(manifest, st, q)
		if err != nil {
			t.Fatal(err)
		}
		avg := 0.0
		for _, v := range rep.FramePSNR {
			avg += v
		}
		avg /= float64(len(rep.FramePSNR))
		if i > 0 && avg >= last {
			t.Fatalf("PSNR must degrade as step grows: q=%d avg=%.3f prev=%.3f", q, avg, last)
		}
		last = avg
	}
}
