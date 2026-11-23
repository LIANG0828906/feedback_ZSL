package tactical_test

import (
	"context"
	"path/filepath"
	"reflect"
	"sync"
	"testing"

	"tacticalworkbench/internal/sample"
	"tacticalworkbench/pkg/tactical"
)

func refEq(a, b []tactical.GridPoint) bool {
	return reflect.DeepEqual(a, b)
}

func TestIncrementalRegionEditReusesUnaffectedCaches(t *testing.T) {
	scenario := baseScenario(t)
	engine := newEngine(t, scenario)
	initial, err := engine.FullRebuild(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	initialRearCost := initial.Paths["A-REAR"].Cost
	initialRearCells := append([]tactical.GridPoint(nil), initial.Paths["A-REAR"].Cells...)
	result, err := engine.ApplyRegionEdit(context.Background(), "bridge-lock", true)
	if err != nil {
		t.Fatalf("region edit failed: %v", err)
	}
	if !result.Consistency.Equal {
		t.Fatalf("incremental result was not marked consistent")
	}
	if result.Stats.CacheHits != 5 || result.Stats.CacheMisses != 1 {
		t.Fatalf("unexpected path cache budget: %+v", result.Stats)
	}
	if result.Stats.ReusedIntervals != 3 {
		t.Fatalf("expected BRAVO trace intervals to be reused, got %+v", result.Stats)
	}
	var pathInvalid, simInvalid int
	for _, entry := range result.Invalidation.Entries {
		switch entry.Kind {
		case tactical.InvalidatePath:
			pathInvalid++
			if entry.MemberID != "A-REAR" {
				t.Fatalf("unexpected path invalidation: %+v", entry)
			}
		case tactical.InvalidateSimulation:
			simInvalid++
			if entry.MemberID != "A-LEAD" && entry.MemberID != "A-RIGHT" && entry.MemberID != "A-REAR" {
				t.Fatalf("unexpected simulation invalidation: %+v", entry)
			}
		}
	}
	if pathInvalid != 1 || simInvalid != 3 {
		t.Fatalf("unexpected invalidation report: %+v", result.Invalidation)
	}
	if refEq(initialRearCells, result.Snapshot.Paths["A-REAR"].Cells) {
		t.Fatalf("expected rear path to route around the lock, cost %d -> %d", initialRearCost, result.Snapshot.Paths["A-REAR"].Cost)
	}
	if result.Snapshot.Paths["A-LEAD"].Cost != initial.Paths["A-LEAD"].Cost {
		t.Fatalf("unaffected leader path was changed")
	}

	editedScenario := engine.Scenario()
	fullEngine := newEngine(t, &editedScenario)
	full, err := fullEngine.FullRebuild(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if err := tactical.CompareSnapshots(result.Snapshot, full); err != nil {
		t.Fatalf("incremental/full mismatch: %v", err)
	}
}

func TestCheckpointRestoreAndStaleInvalidation(t *testing.T) {
	scenario := baseScenario(t)
	engine := newEngine(t, scenario)
	if _, err := engine.FullRebuild(context.Background()); err != nil {
		t.Fatal(err)
	}
	result, err := engine.ApplyRegionEdit(context.Background(), "bridge-lock", true)
	if err != nil {
		t.Fatal(err)
	}
	dbPath := filepath.Join(t.TempDir(), "checkpoint.db")
	store, err := tactical.OpenCheckpointStore(dbPath)
	if err != nil {
		t.Fatal(err)
	}
	meta, err := engine.SaveCheckpoint(context.Background(), store)
	if err != nil {
		t.Fatal(err)
	}
	if !meta.Valid || meta.CommitSeq != 1 {
		t.Fatalf("unexpected checkpoint metadata: %+v", meta)
	}
	order, err := store.CommitOrder(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(order) != len(result.Snapshot.MemberOrder) {
		t.Fatalf("unexpected commit order length: %v", order)
	}
	for i, id := range result.Snapshot.MemberOrder {
		if order[i] != id {
			t.Fatalf("commit order diverged at %d: want %s got %s", i, id, order[i])
		}
	}
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}

	store, err = tactical.OpenCheckpointStore(dbPath)
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	editedScenario := engine.Scenario()
	resumedEngine := newEngine(t, &editedScenario)
	loadedMeta, err := resumedEngine.LoadCheckpoint(context.Background(), store)
	if err != nil {
		t.Fatal(err)
	}
	if !loadedMeta.Valid {
		t.Fatalf("expected valid checkpoint, got %+v", loadedMeta)
	}
	resumed := resumedEngine.Snapshot()
	if resumed == nil {
		t.Fatal("expected resumed snapshot")
	}
	if err := tactical.CompareSnapshots(result.Snapshot, resumed); err != nil {
		t.Fatalf("resumed snapshot differs from incremental: %v", err)
	}
	fullEngine := newEngine(t, &editedScenario)
	full, err := fullEngine.FullRebuild(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if err := tactical.CompareSnapshots(resumed, full); err != nil {
		t.Fatalf("resumed snapshot differs from full: %v", err)
	}

	staleEngine := newEngine(t, scenario)
	staleMeta, err := staleEngine.LoadCheckpoint(context.Background(), store)
	if err != nil {
		t.Fatal(err)
	}
	if staleMeta.Valid || staleMeta.StaleReason == "" {
		t.Fatalf("expected stale checkpoint, got %+v", staleMeta)
	}
	rebuiltMeta, usedCheckpoint, err := staleEngine.ResumeOrRebuild(context.Background(), store)
	if err != nil {
		t.Fatal(err)
	}
	if usedCheckpoint || !rebuiltMeta.Valid {
		t.Fatalf("stale checkpoint was not fully rebuilt: used=%v meta=%+v", usedCheckpoint, rebuiltMeta)
	}
	rebuilt := staleEngine.Snapshot()
	fullBase := newEngine(t, scenario)
	fullBaseSnapshot, err := fullBase.FullRebuild(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if err := tactical.CompareSnapshots(rebuilt, fullBaseSnapshot); err != nil {
		t.Fatalf("stale rebuild differs from full: %v", err)
	}
}

func TestCheckpointWritesAreSerializedAndStable(t *testing.T) {
	scenario := baseScenario(t)
	engine := newEngine(t, scenario)
	snapshot, err := engine.FullRebuild(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	store, err := tactical.OpenCheckpointStore(filepath.Join(t.TempDir(), "concurrent.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	const writers = 6
	var wg sync.WaitGroup
	errorsCh := make(chan error, writers)
	for i := 0; i < writers; i++ {
		wg.Add(1)
		go func(copyIndex int) {
			defer wg.Done()
			clone := *snapshot
			clone.CommitSeq = 0
			clone.SavedAt = snapshot.SavedAt.Add(0)
			_, err := store.Save(context.Background(), &clone)
			errorsCh <- err
		}(i)
	}
	wg.Wait()
	close(errorsCh)
	for err := range errorsCh {
		if err != nil {
			t.Fatalf("concurrent save failed: %v", err)
		}
	}
	order, err := store.CommitOrder(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	memberCount := len(snapshot.MemberOrder)
	if len(order) != writers*memberCount {
		t.Fatalf("expected %d commit rows, got %d", writers*memberCount, len(order))
	}
	for block := 0; block < writers; block++ {
		for i := 0; i < memberCount; i++ {
			if order[block*memberCount+i] != snapshot.MemberOrder[i] {
				t.Fatalf("member writes interleaved in block %d at position %d", block, i)
			}
		}
	}
}

func TestCheckpointVersionMismatchDropsWholeSnapshot(t *testing.T) {
	scenario := baseScenario(t)
	engine := newEngine(t, scenario)
	snapshot, err := engine.FullRebuild(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	baseKey := tactical.CheckpointKey{
		MapHash: snapshot.MapHash, SlotVersion: snapshot.SlotVersion,
		FixedVersion: snapshot.FixedVersion, AlgorithmVersion: snapshot.AlgorithmVersion,
	}
	cases := []struct {
		name   string
		mutate func(*tactical.CheckpointKey)
	}{
		{"map", func(key *tactical.CheckpointKey) { key.MapHash = "changed" }},
		{"slot", func(key *tactical.CheckpointKey) { key.SlotVersion = "changed" }},
		{"fixed", func(key *tactical.CheckpointKey) { key.FixedVersion = "changed" }},
		{"algorithm", func(key *tactical.CheckpointKey) { key.AlgorithmVersion = "changed" }},
	}
	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			dbPath := filepath.Join(t.TempDir(), "mismatch-"+testCase.name+".db")
			store, err := tactical.OpenCheckpointStore(dbPath)
			if err != nil {
				t.Fatal(err)
			}
			defer store.Close()
			if _, err := store.Save(context.Background(), snapshot); err != nil {
				t.Fatal(err)
			}
			key := baseKey
			testCase.mutate(&key)
			loaded, meta, err := store.Load(context.Background(), key)
			if err != nil {
				t.Fatal(err)
			}
			if loaded != nil || meta.Valid || meta.StaleReason == "" {
				t.Fatalf("mismatched checkpoint was partially used: loaded=%v meta=%+v", loaded, meta)
			}
			order, err := store.CommitOrder(context.Background())
			if err != nil {
				t.Fatal(err)
			}
			if len(order) != 0 {
				t.Fatalf("stale checkpoint partitions survived: %v", order)
			}
		})
	}
}

func TestMultipleIncrementalEditsStayConsistent(t *testing.T) {
	scenario := baseScenario(t)
	if _, err := sample.Generate(sample.DefaultSeed); err != nil {
		t.Fatal(err)
	}
	engine := newEngine(t, scenario)
	if _, err := engine.FullRebuild(context.Background()); err != nil {
		t.Fatal(err)
	}
	edits := []func() (*tactical.EditResult, error){
		func() (*tactical.EditResult, error) {
			return engine.ApplyCostEdit(context.Background(), tactical.GridPoint{X: 10, Y: 5}, 4)
		},
		func() (*tactical.EditResult, error) {
			return engine.ApplyRegionEdit(context.Background(), "bridge-lock", true)
		},
		func() (*tactical.EditResult, error) {
			return engine.ApplyRegionEdit(context.Background(), "bridge-lock", false)
		},
	}
	for i, edit := range edits {
		result, err := edit()
		if err != nil {
			t.Fatalf("edit %d failed: %v", i, err)
		}
		if !result.Consistency.Equal {
			t.Fatalf("edit %d was not consistent", i)
		}
	}
	finalScenario := engine.Scenario()
	fullEngine := newEngine(t, &finalScenario)
	full, err := fullEngine.FullRebuild(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if err := tactical.CompareSnapshots(engine.Snapshot(), full); err != nil {
		t.Fatalf("final incremental result differs from full: %v", err)
	}
}
