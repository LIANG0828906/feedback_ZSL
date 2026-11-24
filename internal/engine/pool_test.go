package engine

import (
	"path/filepath"
	"testing"

	"replaykernel/internal/gen"
	"replaykernel/internal/model"
	"replaykernel/internal/store"
)

func TestPoolBackpressureAndDeterminism(t *testing.T) {
	dir := t.TempDir()
	specs := []gen.MatchSpec{
		{MatchID: "p1", Turns: 60, Units: 4},
		{MatchID: "p2", Turns: 60, Units: 4},
		{MatchID: "p3", Turns: 60, Units: 4},
	}
	if _, err := gen.Generate(gen.Spec{OutDir: dir, Seed: 5, MatchSpecs: specs}); err != nil {
		t.Fatal(err)
	}

	params := model.DefaultParams()
	params.MaxParallel = 2
	params.WorkerBufferCap = 1 // 强制最小缓冲，验证阻塞排队
	pool := NewPool(params)

	for _, ms := range specs {
		raw := mustRead(t, filepath.Join(dir, "replays", ms.MatchID+".json"))
		replay, err := LoadReplay(raw)
		if err != nil {
			t.Fatal(err)
		}
		truthPath := filepath.Join(dir, "truth", ms.MatchID+".truth.json")
		truth := mustRead(t, truthPath)
		dbPath := filepath.Join(dir, ms.MatchID+".db")
		pool.Submit(MatchJob{
			Replay:    replay,
			RawReplay: raw,
			TruthHash: HashTruth(mustTruth(t, truth)),
			Mode:      ModeLive,
			OpenDB: func() (*store.Store, error) {
				return store.Open(dbPath)
			},
		})
	}
	outcomes := pool.Wait()
	if len(outcomes) != len(specs) {
		t.Fatalf("outcomes=%d", len(outcomes))
	}
	for _, o := range outcomes {
		if o.Err != nil {
			t.Fatalf("match %s: %v", o.MatchID, o.Err)
		}
	}
	// 结果按 ID 排序，保证可重复。
	if outcomes[0].MatchID != "p1" || outcomes[2].MatchID != "p3" {
		t.Fatalf("non-deterministic ordering: %+v", outcomes)
	}
}
