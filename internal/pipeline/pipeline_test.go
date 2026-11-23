package pipeline

import (
	"context"
	"path/filepath"
	"testing"

	"vqa/internal/metrics"
	"vqa/internal/sample"
	"vqa/internal/store"
)

func fixture(t *testing.T, dir string) (*sample.Sequence, metrics.Params) {
	t.Helper()
	cfg := sample.GenConfig{
		Seed: 7, Width: 32, Height: 32, Channels: 1, Frames: 3,
		BlockSize: 8, Qualities: []int{90, 70, 50}, SampleFrames: []int{0, 2},
		GeneratedAt: "test",
	}
	gen, err := sample.Generate(cfg)
	if err != nil {
		t.Fatal(err)
	}
	if err := sample.WritePersisted(dir, gen); err != nil {
		t.Fatal(err)
	}
	seq, err := sample.LoadSequence(dir, 70)
	if err != nil {
		t.Fatal(err)
	}
	params := metrics.DefaultParams(8, 32, 32, 1)
	return seq, params
}

func runEngine(t *testing.T, seq *sample.Sequence, p metrics.Params, dir, mode string, workers, crash int) *store.Store {
	t.Helper()
	eng := &Engine{
		Seq:         seq,
		Params:      p,
		StoreDir:    dir,
		Workers:     workers,
		MaxPixelBuf: 4096,
		EdgeTol:     12,
		Crash:       CrashPlan{AfterCommits: crash},
	}
	st, err := eng.Run(context.Background(), Run(mode))
	if err != nil && !IsSimulatedCrash(err) {
		t.Fatalf("engine %s: %v", mode, err)
	}
	if st != nil {
		return st
	}
	// Crash path closed the store; reopen read-only for progress inspection.
	ro, err := store.OpenReadOnly(dir)
	if err != nil {
		t.Fatal(err)
	}
	return ro
}

func TestThreePathsProduceIdenticalStores(t *testing.T) {
	base := t.TempDir()
	data := filepath.Join(base, "data")
	seq, params := fixture(t, data)

	rt := runEngine(t, seq, params, filepath.Join(base, "rt"), "realtime", 2, 0)
	rb := runEngine(t, seq, params, filepath.Join(base, "rb"), "rebuild", 1, 0)
	if err := CompareStores(rt, rb); err != nil {
		t.Fatalf("realtime vs rebuild: %v", err)
	}

	// Crash mid-frame, then resume to completion.
	crashDir := filepath.Join(base, "crash")
	runEngine(t, seq, params, crashDir, "rebuild", 2, 10)
	rs := runEngine(t, seq, params, crashDir, "resume", 2, 0)
	if err := CompareStores(rt, rs); err != nil {
		t.Fatalf("realtime vs resumed: %v", err)
	}
}

func TestParameterChangeInvalidatesSnapshot(t *testing.T) {
	base := t.TempDir()
	data := filepath.Join(base, "data")
	seq, params := fixture(t, data)
	dir := filepath.Join(base, "store")
	runEngine(t, seq, params, dir, "realtime", 1, 0)

	tighter := params
	tighter.Window = 5
	tighter.Stride = 1
	eng := &Engine{Seq: seq, Params: tighter, StoreDir: dir, Workers: 1, MaxPixelBuf: 4096, EdgeTol: 12}
	st, err := eng.Run(context.Background(), Resume)
	if err != nil {
		t.Fatalf("resume must recompute after mismatch: %v", err)
	}
	committed, total := st.Progress()
	if committed != total {
		t.Fatalf("after invalidation expected full recompute, got %d/%d", committed, total)
	}
}
