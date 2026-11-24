package store_test

import (
	"context"
	"fmt"
	"path/filepath"
	"sync"
	"testing"

	"strata/internal/model"
	"strata/internal/store"
)

func TestAtomicSerialCheckpointsAndReopen(t *testing.T) {
	path := filepath.Join(t.TempDir(), "checkpoints.db")
	db, err := store.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	if err := db.BeginRun(context.Background(), "run-a", "exact", 1); err != nil {
		t.Fatal(err)
	}
	const n = 24
	var wg sync.WaitGroup
	errs := make(chan error, n)
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			chunk := fmt.Sprintf("C%02d", i)
			_, err := db.CommitChunk(context.Background(), store.ChunkCheckpoint{
				RunID: "run-a", Version: 1, ChunkID: chunk,
				ParamsHash: "params", ParamsVersion: 1, Algorithm: model.AlgorithmVersion,
				ContentKey: "key-" + chunk, ContentHash: "hash-" + chunk,
				PointIndices: []int{i}, Vertices: []model.MeshVertex{{Position: model.Vec3{X: float64(i)}}},
				Faces: []model.MeshFace{{A: 0, B: 0, C: 0, Chunk: chunk}}, Reason: "test",
			})
			if err != nil {
				errs <- err
			}
		}(i)
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		t.Fatal(err)
	}
	cps, err := db.LoadCheckpoints(context.Background(), "run-a")
	if err != nil {
		t.Fatal(err)
	}
	if len(cps) != n {
		t.Fatalf("checkpoint count=%d", len(cps))
	}
	for i, cp := range cps {
		if cp.Seq != i+1 {
			t.Fatalf("sequence is not serial: index=%d seq=%d", i, cp.Seq)
		}
	}
	_, err = db.CommitChunk(context.Background(), store.ChunkCheckpoint{
		RunID: "run-a", Version: 1, ChunkID: "C00", ParamsHash: "params", ParamsVersion: 1,
		Algorithm: model.AlgorithmVersion, ContentKey: "key-C00", ContentHash: "hash-C00",
	})
	if err == nil || model.AsAppError(err).Code != model.ESTATE {
		t.Fatalf("duplicate committed chunk must return E_STATE, got %v", err)
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}
	db, err = store.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	cps, err = db.LoadCheckpoints(context.Background(), "run-a")
	if err != nil || len(cps) != n {
		t.Fatalf("reopen checkpoints=%d err=%v", len(cps), err)
	}
}
