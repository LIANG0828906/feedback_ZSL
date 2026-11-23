package pipeline

import (
	"context"
	"path/filepath"
	"testing"

	"vqa/internal/errs"
	"vqa/internal/metrics"
	"vqa/internal/sample"
	"vqa/internal/store"
)

func TestCompareLocatesFirstDivergentPixel(t *testing.T) {
	base := t.TempDir()
	data := filepath.Join(base, "data")
	cfg := sample.GenConfig{
		Seed: 3, Width: 16, Height: 16, Channels: 1, Frames: 1,
		BlockSize: 8, Qualities: []int{70}, SampleFrames: []int{0},
		GeneratedAt: "test",
	}
	gen, err := sample.Generate(cfg)
	if err != nil {
		t.Fatal(err)
	}
	if err := sample.WritePersisted(data, gen); err != nil {
		t.Fatal(err)
	}
	seq, err := sample.LoadSequence(data, 70)
	if err != nil {
		t.Fatal(err)
	}
	params := metrics.DefaultParams(8, 16, 16, 1)
	eng := &Engine{Seq: seq, Params: params, StoreDir: filepath.Join(base, "a"), Workers: 1, MaxPixelBuf: 2048, EdgeTol: 12}
	stA, err := eng.Run(context.Background(), Rebuild)
	if err != nil {
		t.Fatal(err)
	}

	// Build a second store, then tamper one reconstructed pixel in block (1,1).
	engB := &Engine{Seq: seq, Params: params, StoreDir: filepath.Join(base, "b"), Workers: 1, MaxPixelBuf: 2048, EdgeTol: 12}
	stB, err := engB.Run(context.Background(), Rebuild)
	if err != nil {
		t.Fatal(err)
	}
	blocks := stB.Blocks()
	for i := range blocks {
		if blocks[i].Frame == 0 && blocks[i].BX == 1 && blocks[i].BY == 1 {
			blocks[i].ReconPixels[0] ^= 0x01
			blocks[i].ReconDigest = store.HashBytes(blocks[i].ReconPixels)
		}
	}
	tampered := filepath.Join(base, "tampered")
	writeTampered(t, stB, tampered)

	ro, err := store.OpenReadOnly(tampered)
	if err != nil {
		t.Fatal(err)
	}
	err = CompareStores(stA, ro)
	if errs.CodeOf(err) != errs.EDiverge {
		t.Fatalf("want E_DIVERGE, got %v", err)
	}
	be := err.(*errs.Error)
	if be.Fields["frame"] != "0" || be.Fields["bx"] != "1" || be.Fields["by"] != "1" {
		t.Fatalf("divergence not located at tampered block: %+v", be.Fields)
	}
	if be.Fields["field"] != "recon_pixels" || be.Fields["pixel_x"] != "8" || be.Fields["pixel_y"] != "8" {
		t.Fatalf("first divergent pixel not located precisely: %+v", be.Fields)
	}
}

func writeTampered(t *testing.T, st *store.Store, dir string) {
	t.Helper()
	fresh, err := store.Create(dir, st.Meta())
	if err != nil {
		t.Fatal(err)
	}
	for i := range st.Blocks() {
		rec := st.Blocks()[i]
		if err := fresh.AppendBlock(&rec); err != nil {
			t.Fatal(err)
		}
	}
	for i := range st.Frames() {
		fr := st.Frames()[i]
		if err := fresh.AppendFrame(&fr); err != nil {
			t.Fatal(err)
		}
	}
	if err := fresh.Close(); err != nil {
		t.Fatal(err)
	}
}
