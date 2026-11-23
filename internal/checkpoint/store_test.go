package checkpoint

import (
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"stitchworkbench/internal/model"
)

func TestAtomicSerialSave(t *testing.T) {
	dir := t.TempDir()
	st := NewStore(dir)
	var wg sync.WaitGroup
	for i := 1; i <= 12; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			rec := model.CheckpointRecord{
				PairIndex: i, PairID: "pair", Homography: model.Identity().Slice(),
				ContentHash: "hash", ParamVersion: 1, AlgorithmVersion: model.AlgorithmVersion,
				CommittedAt: time.Now().UTC(),
			}
			_ = st.Save(model.CheckpointSnapshot{
				Header: model.CheckpointHeader{
					Format: "stitchworkbench.checkpoint/v1", SetID: "set",
					ParamVersion: 1, ParamsHash: "p", BaseContentHash: "base",
					ContentHashes: map[string]string{"a": "b"}, AlgorithmVersion: model.AlgorithmVersion,
				},
				Records: []model.CheckpointRecord{rec},
			})
		}(i)
	}
	wg.Wait()
	snap, err := st.Load()
	if err != nil {
		t.Fatal(err)
	}
	if snap == nil || len(snap.Records) != 1 {
		t.Fatalf("checkpoint unreadable after concurrent writes: %#v", snap)
	}
	if _, err := os.Stat(filepath.Join(dir, "checkpoint.json")); err != nil {
		t.Fatal(err)
	}
}
