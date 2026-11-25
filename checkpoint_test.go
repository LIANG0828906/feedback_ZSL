package iconmask

import (
	"context"
	"path/filepath"
	"sync"
	"testing"
)

func TestCheckpointAtomicSerializedPersistence(t *testing.T) {
	path := filepath.Join(t.TempDir(), "checkpoints.db")
	store, err := OpenCheckpointStore(path)
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	ctx := context.Background()
	checkpoint := Checkpoint{
		RunID:            "run-1",
		ImageID:          "icon-96",
		Block:            Coord{X: 0, Y: 0},
		Round:            0,
		Stage:            StageRadius,
		OutputDigest:     "digest",
		RadiusTableJSON:  `{"segments":[]}`,
		CacheKey:         "cache",
		ContentHash:      "content",
		ParameterVersion: "param",
		AlgorithmVersion: AlgorithmVersion,
		OutputMask:       []byte{1, 0, 1},
	}
	saved, err := store.SaveStageCheckpoint(ctx, checkpoint)
	if err != nil {
		t.Fatal(err)
	}
	if saved.Seq == 0 {
		t.Fatal("expected database-assigned sequence")
	}
	if _, err := store.SaveStageCheckpoint(ctx, checkpoint); err == nil {
		t.Fatal("duplicate stage commit must fail")
	} else {
		assertCode(t, err, CodeCheckpoint)
	}
	count, err := store.Count(ctx, "run-1")
	if err != nil {
		t.Fatal(err)
	}
	if count != 1 {
		t.Fatalf("failed duplicate insert must be atomic, got count %d", count)
	}
	loaded, err := store.LoadCheckpoints(ctx, "run-1")
	if err != nil {
		t.Fatal(err)
	}
	if len(loaded) != 1 || string(loaded[0].OutputMask) != string(checkpoint.OutputMask) || loaded[0].Stage != StageRadius {
		t.Fatalf("unexpected persisted checkpoint: %+v", loaded)
	}
}

func TestConcurrentCheckpointWritesDoNotInterleave(t *testing.T) {
	path := filepath.Join(t.TempDir(), "concurrent.db")
	store, err := OpenCheckpointStore(path)
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	ctx := context.Background()
	var wg sync.WaitGroup
	for i := 0; i < 24; i++ {
		wg.Add(1)
		go func(index int) {
			defer wg.Done()
			_, saveErr := store.SaveStageCheckpoint(ctx, Checkpoint{
				RunID:            "run-concurrent",
				ImageID:          "icon",
				Block:            Coord{X: index, Y: 0},
				Round:            0,
				Stage:            StageRadius,
				OutputDigest:     "digest",
				RadiusTableJSON:  `{"segments":[]}`,
				CacheKey:         "cache",
				ContentHash:      "content",
				ParameterVersion: "param",
				AlgorithmVersion: AlgorithmVersion,
			})
			if saveErr != nil {
				t.Errorf("concurrent checkpoint %d: %v", index, saveErr)
			}
		}(i)
	}
	wg.Wait()
	count, err := store.Count(ctx, "run-concurrent")
	if err != nil {
		t.Fatal(err)
	}
	if count != 24 {
		t.Fatalf("expected 24 serialized checkpoints, got %d", count)
	}
	commits, err := store.CommitSequence(ctx, "run-concurrent")
	if err != nil {
		t.Fatal(err)
	}
	for i := 1; i < len(commits); i++ {
		if commits[i].Seq <= commits[i-1].Seq {
			t.Fatal("checkpoint sequence must be strictly increasing")
		}
	}
}
