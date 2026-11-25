package engine_test

import (
	"path/filepath"
	"testing"

	"replayaudit/internal/engine"
	"replayaudit/internal/errs"
	"replayaudit/internal/gen"
	"replayaudit/internal/model"
	"replayaudit/internal/store"
)

func TestCheckpointResumeParityAllSamples(t *testing.T) {
	p := model.DefaultParams()
	for _, b := range gen.BuildAll() {
		db := filepath.Join(t.TempDir(), b.Replay.ReplayID+".db")

		st, err := store.OpenFile(db)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := engine.RunCheckpointed(b.Replay, p, st, engine.CheckpointOptions{InterruptAfter: 1}); err != engine.ErrInterrupted {
			t.Fatalf("%s: want ErrInterrupted, got %v", b.Replay.ReplayID, err)
		}
		if err := st.Close(); err != nil {
			t.Fatal(err)
		}

		st2, err := store.OpenFile(db)
		if err != nil {
			t.Fatal(err)
		}
		res, err := engine.RunCheckpointed(b.Replay, p, st2, engine.CheckpointOptions{})
		if err != nil {
			t.Fatalf("%s resume: %v", b.Replay.ReplayID, err)
		}
		if err := st2.Close(); err != nil {
			t.Fatal(err)
		}

		full, err := engine.SettleFull(b.Replay, p)
		if err != nil {
			t.Fatal(err)
		}
		if mm := engine.CompareReports(full, res); mm != nil {
			t.Fatalf("%s checkpoint parity: %v", b.Replay.ReplayID, mm)
		}
	}
}

func TestCheckpointPurgeOnVersionChange(t *testing.T) {
	p := model.DefaultParams()
	b := gen.BuildAll()[0]
	db := filepath.Join(t.TempDir(), "v.db")
	st, err := store.OpenFile(db)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := engine.RunCheckpointed(b.Replay, p, st, engine.CheckpointOptions{}); err != nil {
		t.Fatal(err)
	}
	_ = st.Close()

	st2, err := store.OpenFile(db)
	if err != nil {
		t.Fatal(err)
	}
	// The runner stamps model.AlgoVersion, so simulate mismatch at the
	// compatibility boundary directly with a different declared algo tag.
	ok, err := st2.CheckCompatible(b.Replay.ReplayID, model.HashReplay(b.Replay),
		model.FormatVersion, engine.ParamsTag(p), "settle-algo-9.9")
	if err != nil {
		t.Fatal(err)
	}
	if ok {
		t.Fatal("algo version mismatch must invalidate snapshot")
	}
	recs, _, _ := st2.LoadSegments(b.Replay.ReplayID)
	if len(recs) != 0 {
		t.Fatalf("expected purge, got %d segments", len(recs))
	}
	_ = st2.Close()
}

func TestCheckpointConflictCodeMatchesFull(t *testing.T) {
	p := model.DefaultParams()
	r := &model.Replay{
		ReplayID: "cp-conflict",
		Entities: []model.EntitySpec{{ID: "a", Side: "blue", MaxHP: 10}, {ID: "b", Side: "red", MaxHP: 10}},
		Sync:     []model.SyncPoint{{ID: "s1", At: 50}},
		Events: []*model.Event{
			{ID: "e1", Kind: "damage", At: 0, Fault: true, Casters: []string{"a"}, Targets: []string{"b"}, Amount: 1},
		},
	}
	full, fullErr := engine.SettleFull(r, p)
	if errs.CodeOf(fullErr) != errs.EConflict {
		t.Fatalf("full want E_CONFLICT, got %v", fullErr)
	}

	st, err := store.OpenFile(filepath.Join(t.TempDir(), "c.db"))
	if err != nil {
		t.Fatal(err)
	}
	cp, cpErr := engine.RunCheckpointed(r, p, st, engine.CheckpointOptions{})
	_ = st.Close()
	if errs.CodeOf(cpErr) != errs.EConflict {
		t.Fatalf("checkpoint want E_CONFLICT, got %v", cpErr)
	}
	if mm := engine.CompareReports(full, cp); mm != nil {
		t.Fatalf("conflict reports differ: %v", mm)
	}
}
