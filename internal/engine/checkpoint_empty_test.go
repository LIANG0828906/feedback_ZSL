package engine_test

import (
	"path/filepath"
	"testing"

	"replayaudit/internal/engine"
	"replayaudit/internal/model"
	"replayaudit/internal/store"
)

func TestCheckpointEmptyLeadingSegment(t *testing.T) {
	p := model.DefaultParams() // SegmentUnits=6000
	r := &model.Replay{
		ReplayID: "late-start",
		Entities: []model.EntitySpec{
			{ID: "a", Side: "blue", MaxHP: 100, MaxMP: 100},
			{ID: "b", Side: "red", MaxHP: 100, MaxMP: 100},
		},
		Events: []*model.Event{
			{ID: "e1", Kind: "damage", At: 7000, Casters: []string{"a"}, Targets: []string{"b"}, Amount: 20},
			{ID: "e2", Kind: "damage", At: 13000, Casters: []string{"a"}, Targets: []string{"b"}, Amount: 30},
		},
	}

	db := filepath.Join(t.TempDir(), "late.db")
	st, err := store.OpenFile(db)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := engine.RunCheckpointed(r, p, st, engine.CheckpointOptions{InterruptAfter: 1}); err != engine.ErrInterrupted {
		t.Fatalf("want interrupt after empty segment 0, got %v", err)
	}
	_ = st.Close()

	st2, err := store.OpenFile(db)
	if err != nil {
		t.Fatal(err)
	}
	res, err := engine.RunCheckpointed(r, p, st2, engine.CheckpointOptions{})
	_ = st2.Close()
	if err != nil {
		t.Fatal(err)
	}
	full, _ := engine.SettleFull(r, p)
	if mm := engine.CompareReports(full, res); mm != nil {
		t.Fatalf("empty-leading-segment parity: %v", mm)
	}
	if len(res.CommitSequence) < 2 || len(res.CommitSequence[0].EventIDs) != 0 {
		t.Fatalf("expected empty segment 0 commit, got %+v", res.CommitSequence)
	}
}
