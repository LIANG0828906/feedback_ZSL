package audiofp

import (
	"context"
	"path/filepath"
	"testing"
)

func TestVerifierDetectsTamperedPersistedPayload(t *testing.T) {
	ctx := context.Background()
	r := checkpointRecipe()
	assets := buildFixtureAssets()
	store, err := OpenStore(filepath.Join(t.TempDir(), "verify.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	if _, err := BuildCheckpoint(ctx, store, "p", assets, r, SnapshotFresh, BuildOptions{
		ShardFrames: 4, Extraction: ExtractionOptions{Workers: 2},
	}); err != nil {
		t.Fatal(err)
	}
	truth := Truth{FinalRecipe: r}
	for _, asset := range assets {
		analysis, _, err := AnalyzeAsset(ctx, asset, r, ExtractionOptions{Workers: 1})
		if err != nil {
			t.Fatal(err)
		}
		at := AssetTruth{AssetID: asset.ID, ContentHash: asset.ContentHash(), TotalSamples: len(asset.Samples)}
		for _, frame := range analysis.Frames {
			at.Frames = append(at.Frames, frame.ToTruth())
		}
		truth.Assets = append(truth.Assets, at)
	}
	if _, err := store.db.Exec(`UPDATE cache_shards SET payload_digest='tampered' WHERE rowid=1`); err != nil {
		t.Fatal(err)
	}
	if _, err := VerifyPersistedSnapshot(ctx, store, "p", r, truth); CodeOf(err) != CodeDiff {
		t.Fatalf("got %v, want E_DIFF", err)
	}
}
