package engine

import (
	"testing"

	"roomreverb/internal/config"
	"roomreverb/internal/diff"
	"roomreverb/internal/model"
	"roomreverb/internal/sample"
)

func testConfig() config.Config {
	cfg := config.Default()
	cfg.LevelPath = ""
	cfg.DatabasePath = ""
	cfg.MaxParallelism = 2
	cfg.SampleBufferLimit = 8
	return cfg
}

func TestDoorAddAndWallDragIncrementalMatchesFull(t *testing.T) {
	level := sample.Generate(42)
	cfg := testConfig()
	eng := New(cfg, level, nil)
	if _, err := eng.RunBaseline(); err != nil {
		t.Fatalf("baseline: %v", err)
	}

	plan, err := eng.AddDoor(model.Door{
		ID: "D14", WallID: "W14",
		A: model.Point{X: 180, Y: 340}, B: model.Point{X: 220, Y: 340}, Kind: "door",
	})
	if err != nil {
		t.Fatalf("add door: %v", err)
	}
	if !contains(plan.InvalidRooms, "R1") || !contains(plan.InvalidRooms, "R4") {
		t.Fatalf("expected R1/R4 topology invalidation, got %+v", plan.InvalidRooms)
	}
	incremental, merr := eng.RecomputeWithPlan(plan)
	if merr != nil {
		t.Fatalf("recompute after add: %v", merr)
	}
	full, ferr := ComputeFull(eng.Level(), cfg, nil)
	if ferr != nil {
		t.Fatal(ferr)
	}
	if derr := diff.CompareFullResults(full, incremental); derr != nil {
		t.Fatalf("add-door diff: %v details=%+v", derr, derr.Details)
	}

	wallPlan, merr := eng.MoveWallLine("W1L", 20)
	if merr != nil {
		t.Fatalf("move wall: %v", merr)
	}
	if len(wallPlan.InvalidRooms) != 0 || len(wallPlan.InvalidListeners) != 0 {
		t.Fatalf("outer geometry move without acoustic path change should reuse caches, rooms=%v listeners=%v", wallPlan.InvalidRooms, wallPlan.InvalidListeners)
	}
	movedIncremental, merr := eng.RecomputeWithPlan(wallPlan)
	if merr != nil {
		t.Fatalf("recompute after wall move: %v", merr)
	}
	movedFull, ferr := ComputeFull(eng.Level(), cfg, nil)
	if ferr != nil {
		t.Fatal(ferr)
	}
	if derr := diff.CompareFullResults(movedFull, movedIncremental); derr != nil {
		t.Fatalf("wall-move diff: %v details=%+v", derr, derr.Details)
	}
}

func TestDeleteBridgeUnreachableThenRestore(t *testing.T) {
	eng := New(testConfig(), sample.Generate(42), nil)
	if _, err := eng.RunBaseline(); err != nil {
		t.Fatal(err)
	}
	plan, merr := eng.DeleteDoor("D25")
	if merr != nil {
		t.Fatal(merr)
	}
	if _, err := eng.RecomputeWithPlan(plan); err == nil || err.Code != model.CodeUnreach {
		t.Fatalf("expected E_UNREACH, got %#v", err)
	}
	snapshot := eng.Snapshot()
	failed := 0
	for _, room := range snapshot.States {
		if room.State == StateFailed {
			failed++
		}
	}
	if failed == 0 || snapshot.LastError == nil || snapshot.LastError.Code != model.CodeUnreach {
		t.Fatalf("expected failed room state and E_UNREACH snapshot, failed=%d err=%+v", failed, snapshot.LastError)
	}

	restorePlan, merr := eng.AddDoor(model.Door{
		ID: "D25", WallID: "W25", Kind: "opening",
		A: model.Point{X: 500, Y: 340}, B: model.Point{X: 540, Y: 340},
	})
	if merr != nil {
		t.Fatal(merr)
	}
	incremental, merr := eng.RecomputeWithPlan(restorePlan)
	if merr != nil {
		t.Fatalf("restore recompute: %v", merr)
	}
	full, err := ComputeFull(eng.Level(), testConfig(), nil)
	if err != nil {
		t.Fatal(err)
	}
	if derr := diff.CompareFullResults(full, incremental); derr != nil {
		t.Fatalf("restore diff: %v", derr.Details)
	}
}

func TestIllegalStateTransitionKeepsOriginalState(t *testing.T) {
	eng := New(testConfig(), sample.Generate(42), nil)
	before := eng.Snapshot().States[0].State
	err := eng.Transition("R1", StateCommitted)
	if err == nil || err.Code != model.CodeState {
		t.Fatalf("expected E_STATE, got %#v", err)
	}
	after := eng.Snapshot().States[0].State
	if before != StatePending || after != StatePending {
		t.Fatalf("state changed: before=%s after=%s", before, after)
	}

	if _, err := eng.RunBaseline(); err != nil {
		t.Fatal(err)
	}
	err = eng.Transition("R1", StatePending)
	if err == nil || err.Code != model.CodeState {
		t.Fatalf("committed must be terminal for ordinary transitions, got %#v", err)
	}
	if state := eng.Snapshot().States[0].State; state != StateCommitted {
		t.Fatalf("illegal transition changed state to %s", state)
	}
	if err := eng.OpenDraft("R1"); err != nil {
		t.Fatalf("OpenDraft should start a new revision: %v", err)
	}
	state := eng.Snapshot().States[0]
	if state.State != StatePending || state.Revision != 2 {
		t.Fatalf("expected pending revision 2 after draft, got %+v", state)
	}
}

func contains(items []string, target string) bool {
	for _, item := range items {
		if item == target {
			return true
		}
	}
	return false
}
