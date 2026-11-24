package store_test

import (
	"context"
	"path/filepath"
	"sync"
	"testing"

	"economycore/internal/core"
	"economycore/internal/store"
)

func storeScenario(t *testing.T) *core.Scenario {
	t.Helper()
	scene := &core.Scenario{
		SchemaVersion: core.SchemaVersion,
		ID:            "store_scene",
		Seed:          1,
		TickUnitMS:    100,
		ParamsVersion: "params/v1",
		FinalTick:     8,
		Nodes: []core.Node{{
			ID: "n", Type: core.NodeFarm, Active: true,
			Workers: 1, MaxWorkers: 2,
			Outputs: map[string]int64{"grain": 1000},
			Inputs:  map[string]int64{}, Capacity: map[string]int64{"grain": 100000},
			WorkersRequired: 1, InitialStock: map[string]int64{"grain": 0},
			Upgrades: []core.Upgrade{},
		}},
		Intents: []core.Intent{{
			Seq: 0, Timestamp: 1, Type: core.IntentAssignWorker, NodeID: "n", Workers: intPtr(1),
		}},
	}
	if err := core.ValidateScenario(scene); err != nil {
		t.Fatal(err)
	}
	h, err := core.ComputeSceneHash(scene)
	if err != nil {
		t.Fatal(err)
	}
	scene.SceneHash = h
	return scene
}

func buildCheckpoint(t *testing.T, scene *core.Scenario, index int, start, end int64) core.WindowCheckpoint {
	t.Helper()
	eng, err := core.NewEngine(scene)
	if err != nil {
		t.Fatal(err)
	}
	if err := eng.AdvanceTo(start); err != nil {
		t.Fatal(err)
	}
	startOffset := eng.State().IntentOffset
	if err := eng.AdvanceTo(end); err != nil {
		t.Fatal(err)
	}
	stateJSON, err := eng.State().MarshalCanonical()
	if err != nil {
		t.Fatal(err)
	}
	return core.WindowCheckpoint{
		ScenarioID: scene.ID, WindowIndex: index,
		StartTick: start, EndTick: end,
		StartIntentOffset: startOffset, EndIntentOffset: eng.State().IntentOffset,
		State: eng.CloneState(), StateJSON: stateJSON, StateHash: core.HashBytes(stateJSON),
		ParamsVersion: scene.ParamsVersion, SceneHash: scene.SceneHash,
		AlgorithmVersion: core.AlgorithmVersion,
	}
}

func TestCheckpointWritesAreOrderedAndSerial(t *testing.T) {
	scene := storeScenario(t)
	st, err := store.Open(filepath.Join(t.TempDir(), "ordered.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	ctx := context.Background()
	if err := st.CommitWindow(ctx, buildCheckpoint(t, scene, 1, 2, 4)); core.CodeOf(err) != core.CodeCheckpt {
		t.Fatalf("out-of-order write code=%s: %v", core.CodeOf(err), err)
	}
	if err := st.CommitWindow(ctx, buildCheckpoint(t, scene, 0, 0, 2)); err != nil {
		t.Fatal(err)
	}
	cp1 := buildCheckpoint(t, scene, 1, 2, 4)
	var wg sync.WaitGroup
	errs := make(chan error, 2)
	for i := 0; i < 2; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			errs <- st.CommitWindow(ctx, cp1)
		}()
	}
	wg.Wait()
	close(errs)
	ok, rejected := 0, 0
	for err := range errs {
		if err == nil {
			ok++
		} else if core.CodeOf(err) == core.CodeCheckpt {
			rejected++
		}
	}
	if ok != 1 || rejected != 1 {
		t.Fatalf("expected one serialized writer, ok=%d rejected=%d", ok, rejected)
	}
	if n, err := st.Count(ctx, scene.ID); err != nil || n != 2 {
		t.Fatalf("count=%d err=%v", n, err)
	}
}

func TestHashMismatchInvalidatesEntireCheckpointSet(t *testing.T) {
	scene := storeScenario(t)
	st, err := store.Open(filepath.Join(t.TempDir(), "hash.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	ctx := context.Background()
	if err := st.CommitWindow(ctx, buildCheckpoint(t, scene, 0, 0, 2)); err != nil {
		t.Fatal(err)
	}
	changed, err := core.CloneScenario(scene)
	if err != nil {
		t.Fatal(err)
	}
	changed.SceneHash = "different"
	resume, err := st.LoadResume(ctx, changed)
	if err != nil {
		t.Fatal(err)
	}
	if !resume.Reset {
		t.Fatal("hash mismatch did not reset")
	}
	if n, err := st.Count(ctx, scene.ID); err != nil || n != 0 {
		t.Fatalf("stale rows remain: count=%d err=%v", n, err)
	}
}

func intPtr(v int) *int { return &v }
