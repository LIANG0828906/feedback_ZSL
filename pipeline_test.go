package silhouette

import (
	"context"
	"path/filepath"
	"testing"
)

func TestThreePathsArePointForPointIdentical(t *testing.T) {
	ctx := context.Background()
	scene := GenerateScene(DefaultSeed)
	dir := t.TempDir()
	cfg := RunConfig{Workers: 3, VerticesPerWorker: DefaultVerticesPerWorker}
	realStore, err := OpenCheckpointStore(filepath.Join(dir, "real.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer realStore.Close()
	realtime, err := RunRealtime(ctx, scene, realStore, cfg)
	if err != nil {
		t.Fatal(err)
	}
	resumeStore, err := OpenCheckpointStore(filepath.Join(dir, "resume.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer resumeStore.Close()
	stopCfg := cfg
	stopCfg.StopAfterCommits = 2
	partial, err := RunRealtime(ctx, scene, resumeStore, stopCfg)
	if err != nil {
		t.Fatal(err)
	}
	if !partial.Interrupted || len(partial.Commits) != 2 {
		t.Fatalf("unexpected interrupted result: interrupted=%t commits=%d", partial.Interrupted, len(partial.Commits))
	}
	resumed, err := RunResume(ctx, scene, resumeStore, cfg)
	if err != nil {
		t.Fatal(err)
	}
	if len(resumed.ReusedTiles) != 2 {
		t.Fatalf("resume reused %d tiles want 2", len(resumed.ReusedTiles))
	}
	fullStore, err := OpenCheckpointStore(filepath.Join(dir, "full.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer fullStore.Close()
	full, err := RunFullRecompute(ctx, scene, fullStore, cfg)
	if err != nil {
		t.Fatal(err)
	}
	if report := CompareRuns(realtime, resumed); !report.Consistent {
		t.Fatalf("resume mismatch: %+v", report)
	}
	if report := CompareRuns(realtime, full); !report.Consistent {
		t.Fatalf("full mismatch: %+v", report)
	}
	checkpoints, err := resumeStore.ListCheckpoints(ctx, scene.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(checkpoints) != len(scene.Tiles) {
		t.Fatalf("checkpoints=%d want %d", len(checkpoints), len(scene.Tiles))
	}
	commits, err := resumeStore.ListCommits(ctx, scene.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(commits) != len(scene.Tiles) {
		t.Fatalf("commit rows=%d want %d (two from interrupted run plus two resumed)", len(commits), len(scene.Tiles))
	}
	for i, commit := range commits {
		if commit.Seq != int64(i+1) {
			t.Fatalf("commit sequence not monotonic: %+v", commits)
		}
	}
}

func TestCompareRunsLocatesFirstMismatchingTileAndVertex(t *testing.T) {
	ctx := context.Background()
	scene := GenerateScene(DefaultSeed)
	store, err := OpenCheckpointStore(filepath.Join(t.TempDir(), "cmp.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	run, err := RunRealtime(ctx, scene, store, RunConfig{Workers: 2, VerticesPerWorker: DefaultVerticesPerWorker})
	if err != nil {
		t.Fatal(err)
	}
	changed := *run
	changedAssembly := *run.Assembly
	changedAssembly.Tiles = append([]TileAssembly(nil), run.Assembly.Tiles...)
	target := &changedAssembly.Tiles[0]
	targetCopy := *target
	targetCopy.Segments = append([]TileSegment(nil), target.Segments...)
	targetCopy.Segments[0].Vertices = append([]SampleVertex(nil), target.Segments[0].Vertices...)
	targetCopy.Segments[0].Vertices[2].X++
	changedAssembly.Tiles[0] = targetCopy
	changed.Assembly = &changedAssembly
	report := CompareRuns(run, &changed)
	if report.Consistent {
		t.Fatal("expected mismatch")
	}
	if report.FirstMismatchTile != run.Assembly.Tiles[0].TileID || report.FirstMismatchVertex != 2 || report.FirstMismatchKind != "tile_vertex" {
		t.Fatalf("unexpected first mismatch: %+v", report)
	}
}

func TestCheckpointCommitRowsContainRequiredFields(t *testing.T) {
	ctx := context.Background()
	scene := GenerateScene(DefaultSeed)
	store, err := OpenCheckpointStore(filepath.Join(t.TempDir(), "fields.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	run, err := RunRealtime(ctx, scene, store, RunConfig{Workers: 2, VerticesPerWorker: DefaultVerticesPerWorker})
	if err != nil {
		t.Fatal(err)
	}
	checkpoints, err := store.ListCheckpoints(ctx, scene.ID)
	if err != nil {
		t.Fatal(err)
	}
	expectedOrder := tileOrderIDs(scene, run.Assembly)
	for i, cp := range checkpoints {
		if cp.TileID != expectedOrder[i] {
			t.Fatalf("checkpoint order=%s want %s", cp.TileID, expectedOrder[i])
		}
		if cp.VertexDigest == "" || cp.ContentHash == "" || cp.CacheKey == "" || cp.AlgorithmVersion == "" || cp.ToleranceVersion == "" || cp.BudgetVersion == "" || len(cp.DependencySet) == 0 {
			t.Fatalf("checkpoint missing required fields: %+v", cp)
		}
		if cp.MaxChordHeight <= 0 {
			t.Fatalf("checkpoint max chord height missing: %+v", cp)
		}
	}
}

func TestFullRecomputeRestartsSceneCommitSequence(t *testing.T) {
	ctx := context.Background()
	scene := GenerateScene(DefaultSeed)
	store, err := OpenCheckpointStore(filepath.Join(t.TempDir(), "restart.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	cfg := RunConfig{Workers: 2, VerticesPerWorker: DefaultVerticesPerWorker}
	if _, err := RunRealtime(ctx, scene, store, cfg); err != nil {
		t.Fatal(err)
	}
	full, err := RunFullRecompute(ctx, scene, store, cfg)
	if err != nil {
		t.Fatal(err)
	}
	for i, commit := range full.Commits {
		if commit.Seq != int64(i+1) {
			t.Fatalf("full recompute commit sequence=%d want %d", commit.Seq, i+1)
		}
	}
	commits, err := store.ListCommits(ctx, scene.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(commits) != len(scene.Tiles) {
		t.Fatalf("commit rows=%d want %d after clear+recompute", len(commits), len(scene.Tiles))
	}
	for i, commit := range commits {
		if commit.Seq != int64(i+1) {
			t.Fatalf("persisted sequence=%d want %d", commit.Seq, i+1)
		}
	}
}
