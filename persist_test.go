package ortho

import (
	"context"
	"os"
	"path/filepath"
	"testing"
)

func runAndSave(t *testing.T, dir string) (*Pipeline, string, *StageParams) {
	t.Helper()
	params := DefaultStageParams()
	src := genTerrainRGB(SampleSeed + 1)
	srcHash := ContentHash(src)
	stages := BuildStages(params)
	pipe := NewPipeline(stages, testCfg(), nil)
	if _, err := pipe.Run(context.Background(), src, srcHash); err != nil {
		t.Fatal(err)
	}
	store := NewStore(dir)
	snap := BuildSnapshot(pipe.Version(), srcHash, stages, pipe.Cache)
	if err := store.Save(snap, CollectImages(stages, pipe.Cache)); err != nil {
		t.Fatal(err)
	}
	return pipe, srcHash, params
}

func TestSnapshotRoundTrip(t *testing.T) {
	dir := t.TempDir()
	pipe, srcHash, params := runAndSave(t, dir)
	store := NewStore(dir)
	stages := BuildStages(params)
	snap, imgs, err := store.Load(pipe.Version(), srcHash)
	if err != nil {
		t.Fatal(err)
	}
	if snap == nil {
		t.Fatal("expected snapshot to load")
	}
	if len(snap.Stages) != 3 || len(imgs) != 3 {
		t.Fatalf("snapshot stages = %d, images = %d", len(snap.Stages), len(imgs))
	}
	// 恢复后全部阶段复用，不再重算。
	cache := NewCache()
	RestoreCache(snap, imgs, cache)
	pipe2 := NewPipeline(stages, testCfg(), cache)
	res, perr := pipe2.Run(context.Background(), genTerrainRGB(SampleSeed+1), srcHash)
	if perr != nil {
		t.Fatal(perr)
	}
	for _, r := range res.Reports {
		if !r.Reused {
			t.Fatalf("stage %s not reused after restore", r.Stage)
		}
	}
}

func TestSnapshotVersionMismatchDiscardsAll(t *testing.T) {
	dir := t.TempDir()
	_, srcHash, _ := runAndSave(t, dir)
	store := NewStore(dir)
	snap, imgs, err := store.Load("homography/v9|radial/v9|affine/v9", srcHash)
	if err != nil {
		t.Fatal(err)
	}
	if snap != nil || imgs != nil {
		t.Fatal("version mismatch must discard snapshot")
	}
	entries, _ := os.ReadDir(dir)
	if len(entries) != 0 {
		t.Fatalf("snapshot dir not fully discarded, %d entries left", len(entries))
	}
}

func TestSnapshotSourceHashMismatchDiscardsAll(t *testing.T) {
	dir := t.TempDir()
	pipe, _, params := runAndSave(t, dir)
	store := NewStore(dir)
	snap, _, err := store.Load(pipe.Version(), "deadbeef")
	if err != nil {
		t.Fatal(err)
	}
	if snap != nil {
		t.Fatal("source hash mismatch must discard snapshot")
	}
	_ = params
}

func TestSnapshotCorruptedStageFileDiscardsAll(t *testing.T) {
	dir := t.TempDir()
	pipe, srcHash, _ := runAndSave(t, dir)
	// 损坏一个阶段文件：整体作废，禁止部分混用。
	if err := os.WriteFile(filepath.Join(dir, "radial.png"), []byte("corrupted"), 0o644); err != nil {
		t.Fatal(err)
	}
	store := NewStore(dir)
	snap, imgs, err := store.Load(pipe.Version(), srcHash)
	if err != nil {
		t.Fatal(err)
	}
	if snap != nil || imgs != nil {
		t.Fatal("corrupted stage file must discard the whole snapshot")
	}
	entries, _ := os.ReadDir(dir)
	if len(entries) != 0 {
		t.Fatalf("partial state left behind: %d entries", len(entries))
	}
}

func TestSnapshotDeterministicKeys(t *testing.T) {
	dir1, dir2 := t.TempDir(), t.TempDir()
	p1, h1, _ := runAndSave(t, dir1)
	p2, h2, _ := runAndSave(t, dir2)
	if h1 != h2 {
		t.Fatal("source hash not deterministic")
	}
	s1 := BuildSnapshot(p1.Version(), h1, BuildStages(DefaultStageParams()), p1.Cache)
	s2 := BuildSnapshot(p2.Version(), h2, BuildStages(DefaultStageParams()), p2.Cache)
	for i := range s1.Stages {
		if s1.Stages[i].Key != s2.Stages[i].Key {
			t.Fatalf("stage %s key not deterministic", s1.Stages[i].ID)
		}
		if s1.Stages[i].ContentHash != s2.Stages[i].ContentHash {
			t.Fatalf("stage %s content hash not deterministic", s1.Stages[i].ID)
		}
	}
}
