package audiofp

import (
	"context"
	"errors"
	"path/filepath"
	"testing"
)

func checkpointRecipe() Recipe {
	return Recipe{FrameLen: 64, FrameShift: 32, Bands: 6, QuantBits: 4,
		SimilarityThreshold: 0.4, CandidateLimit: 8}
}

func buildFixtureAssets() []PCMAsset {
	return []PCMAsset{
		testAsset("a0", 900, 0),
		testAsset("a1", 1100, 0),
		testAsset("a2", 750, 0),
	}
}

func TestCheckpointCancellationResumeAndVersionInvalidation(t *testing.T) {
	ctx := context.Background()
	r := checkpointRecipe()
	assets := buildFixtureAssets()
	path := filepath.Join(t.TempDir(), "resume.db")
	store, err := OpenStore(path)
	if err != nil {
		t.Fatal(err)
	}
	opts := BuildOptions{ShardFrames: 4, Extraction: ExtractionOptions{Workers: 3}}
	partial, err := BuildCheckpoint(ctx, store, "p", assets, r, SnapshotFresh,
		BuildOptions{ShardFrames: 4, CancelAfterNewShards: 2, Extraction: ExtractionOptions{Workers: 3}})
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("got %v, want context.Canceled", err)
	}
	if len(partial.Commits) != 2 {
		t.Fatalf("got %d committed shards, want 2", len(partial.Commits))
	}
	_ = store.Close()

	store, err = OpenStore(path)
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	resumed, err := BuildCheckpoint(ctx, store, "p", assets, r, SnapshotResume, opts)
	if err != nil {
		t.Fatal(err)
	}
	if resumed.ReusedShards < 2 {
		t.Fatalf("resume reused %d shards, want at least 2", resumed.ReusedShards)
	}
	fullStore, err := OpenStore(filepath.Join(t.TempDir(), "full.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer fullStore.Close()
	full, err := BuildCheckpoint(ctx, fullStore, "p", assets, r, SnapshotFresh, opts)
	if err != nil {
		t.Fatal(err)
	}
	if err := CompareBuildResults(full, resumed); err != nil {
		t.Fatal(err)
	}

	// Changing content makes the snapshot version invalid. A resume must reset
	// and rebuild everything instead of mixing old shards.
	changed := append([]PCMAsset(nil), assets...)
	changed[0] = changed[0].Clone()
	for i := 100; i < 150; i++ {
		changed[0].Samples[i] = -changed[0].Samples[i]
	}
	changedResume, err := BuildCheckpoint(ctx, store, "p", changed, r, SnapshotResume, opts)
	if err != nil {
		t.Fatal(err)
	}
	if !changedResume.SnapshotReset || changedResume.ReusedShards != 0 {
		t.Fatalf("version mismatch did not force a clean rebuild: reset=%v reused=%d",
			changedResume.SnapshotReset, changedResume.ReusedShards)
	}
	changedFullStore, err := OpenStore(filepath.Join(t.TempDir(), "changed-full.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer changedFullStore.Close()
	changedFull, err := BuildCheckpoint(ctx, changedFullStore, "p", changed, r, SnapshotFresh, opts)
	if err != nil {
		t.Fatal(err)
	}
	if err := CompareBuildResults(changedFull, changedResume); err != nil {
		t.Fatal(err)
	}
}

func TestIncrementalBuildReusesUnaffectedShards(t *testing.T) {
	ctx := context.Background()
	r := checkpointRecipe()
	assets := buildFixtureAssets()
	store, err := OpenStore(filepath.Join(t.TempDir(), "inc.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	opts := BuildOptions{ShardFrames: 4, Extraction: ExtractionOptions{Workers: 3}}
	if _, err := BuildCheckpoint(ctx, store, "p", assets, r, SnapshotFresh, opts); err != nil {
		t.Fatal(err)
	}
	edited := append([]PCMAsset(nil), assets...)
	edited[0] = edited[0].Clone()
	copy(edited[0].Samples[200:260], testAsset("replacement", 60, 0).Samples)
	inc, err := BuildCheckpoint(ctx, store, "p", edited, r, SnapshotIncremental, opts)
	if err != nil {
		t.Fatal(err)
	}
	if inc.ReusedShards == 0 {
		t.Fatal("incremental rebuild did not reuse persisted unaffected shards")
	}
	fullStore, err := OpenStore(filepath.Join(t.TempDir(), "full.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer fullStore.Close()
	full, err := BuildCheckpoint(ctx, fullStore, "p", edited, r, SnapshotFresh, opts)
	if err != nil {
		t.Fatal(err)
	}
	if err := CompareBuildResults(full, inc); err != nil {
		t.Fatal(err)
	}
}

func TestSingleShardFailureLeavesOtherCommitsAndRecovers(t *testing.T) {
	ctx := context.Background()
	r := checkpointRecipe()
	assets := buildFixtureAssets()
	path := filepath.Join(t.TempDir(), "stage.db")
	store, err := OpenStore(path)
	if err != nil {
		t.Fatal(err)
	}
	opts := BuildOptions{
		ShardFrames: 4,
		Extraction: ExtractionOptions{
			Workers: 3,
			Inject:  &Injection{Kind: "stage", AssetID: "a0", Shard: 0, FailOnce: true},
		},
	}
	failed, err := BuildCheckpoint(ctx, store, "p", assets, r, SnapshotFresh, opts)
	if CodeOf(err) != CodeStage {
		t.Fatalf("got %v, want E_STAGE", err)
	}
	if len(failed.Commits) == 0 {
		t.Fatal("other shards were not committed after one shard failed")
	}
	opts.Extraction.Inject = nil
	recovered, err := BuildCheckpoint(ctx, store, "p", assets, r, SnapshotResume, opts)
	if err != nil {
		t.Fatal(err)
	}
	if recovered.ReusedShards < len(failed.Commits) {
		t.Fatalf("resume reused %d shards, want at least %d", recovered.ReusedShards, len(failed.Commits))
	}
	fullStore, err := OpenStore(filepath.Join(t.TempDir(), "full.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer fullStore.Close()
	full, err := BuildCheckpoint(ctx, fullStore, "p", assets, r, SnapshotFresh, BuildOptions{
		ShardFrames: 4, Extraction: ExtractionOptions{Workers: 3},
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := CompareBuildResults(full, recovered); err != nil {
		t.Fatal(err)
	}
}

func TestSingleShardFailOnceAutoRetries(t *testing.T) {
	ctx := context.Background()
	r := checkpointRecipe()
	assets := buildFixtureAssets()
	store, err := OpenStore(filepath.Join(t.TempDir(), "retry.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	build, err := BuildCheckpoint(ctx, store, "p", assets, r, SnapshotFresh, BuildOptions{
		ShardFrames: 4, MaxShardRetries: 1,
		Extraction: ExtractionOptions{
			Workers: 3,
			Inject:  &Injection{Kind: "stage", AssetID: "a0", Shard: 0, FailOnce: true},
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	if build.RetriedShards == 0 {
		t.Fatal("expected a shard retry to be recorded")
	}
	if len(build.Analyses) != len(assets) {
		t.Fatalf("retry produced %d analyses, want %d", len(build.Analyses), len(assets))
	}
}
