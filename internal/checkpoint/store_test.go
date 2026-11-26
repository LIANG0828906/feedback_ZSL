package checkpoint_test

import (
	"path/filepath"
	"testing"

	"volumekernel/internal/checkpoint"
	"volumekernel/internal/model"
)

func testMeta(scene string) checkpoint.Meta {
	return checkpoint.Meta{
		SessionID:        "test-session",
		SceneHash:        scene,
		CameraVersion:    "camera-v1",
		RecipeVersion:    "recipe-v1",
		AlgorithmVersion: model.AlgorithmVersion,
	}
}

func TestAtomicCheckpointAndWholeSessionInvalidation(t *testing.T) {
	store, err := checkpoint.Open(filepath.Join(t.TempDir(), "checkpoint.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	meta := testMeta("scene-a")
	if _, err := store.BeginSession(meta); err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 2; i++ {
		err := store.CommitBlock(checkpoint.BlockRecord{
			Seq:         i,
			X:           i,
			Y:           0,
			RayDigest:   "ray",
			CacheKey:    "key",
			ContentHash: meta.SceneHash,
			Recipe:      meta.RecipeVersion,
			Algorithm:   meta.AlgorithmVersion,
			Block:       model.BlockResult{X: i, Y: 0, Index: i},
		})
		if err != nil {
			t.Fatal(err)
		}
	}
	recs, state, err := store.LoadCommitted(meta)
	if err != nil {
		t.Fatal(err)
	}
	if state.Invalidated || len(recs) != 2 {
		t.Fatalf("unexpected load state: %+v records=%d", state, len(recs))
	}
	changed := testMeta("scene-b")
	state, err = store.BeginSession(changed)
	if err != nil {
		t.Fatal(err)
	}
	if !state.Invalidated {
		t.Fatal("changed scene hash must invalidate the entire session")
	}
	n, err := store.CheckpointCount()
	if err != nil {
		t.Fatal(err)
	}
	if n != 0 {
		t.Fatalf("expected all checkpoint blocks purged, got %d", n)
	}
}

func TestCorruptCheckpointRowInvalidatesWholeSession(t *testing.T) {
	store, err := checkpoint.Open(filepath.Join(t.TempDir(), "checkpoint.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	meta := testMeta("scene-a")
	if _, err := store.BeginSession(meta); err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 2; i++ {
		if err := store.CommitBlock(checkpoint.BlockRecord{
			Seq: i, X: i, Y: 0, RayDigest: "ray", CacheKey: "key",
			ContentHash: meta.SceneHash, Recipe: meta.RecipeVersion,
			Algorithm: meta.AlgorithmVersion, Block: model.BlockResult{X: i, Y: 0, Index: i},
		}); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := store.DB().Exec(`UPDATE checkpoint_blocks SET recipe_version='tampered' WHERE seq=1`); err != nil {
		t.Fatal(err)
	}
	recs, state, err := store.LoadCommitted(meta)
	if err != nil {
		t.Fatal(err)
	}
	if !state.Invalidated || len(recs) != 0 {
		t.Fatalf("expected whole invalidation, state=%+v recs=%d", state, len(recs))
	}
	n, _ := store.CheckpointCount()
	if n != 0 {
		t.Fatalf("expected purged rows, got %d", n)
	}
}

func TestBlockCacheRoundTripAndVersionGuard(t *testing.T) {
	store, err := checkpoint.Open(filepath.Join(t.TempDir(), "cache.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	block := model.BlockResult{X: 2, Y: 3, Index: 11, CacheKey: "cache-key", ContentHash: "content"}
	if err := store.PutCache("cache-key", "param-v1", "content", model.AlgorithmVersion, block, []string{"voxel:a:0,0,0"}); err != nil {
		t.Fatal(err)
	}
	got, deps, ok, err := store.GetCache("cache-key", "param-v1", model.AlgorithmVersion)
	if err != nil || !ok {
		t.Fatalf("cache miss: ok=%v err=%v", ok, err)
	}
	if got.X != 2 || got.Y != 3 || len(deps) == 0 {
		t.Fatalf("bad cached block: %+v deps=%v", got, deps)
	}
	if _, _, ok, _ := store.GetCache("cache-key", "param-v2", model.AlgorithmVersion); ok {
		t.Fatal("parameter version mismatch should miss")
	}
}
