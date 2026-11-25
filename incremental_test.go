package replaybench_test

import (
	"context"
	"testing"

	"replaybench/internal/checkpoint"
	"replaybench/internal/diff"
	"replaybench/internal/engine"
	"replaybench/internal/graph"
	"replaybench/internal/model"
)

func TestWeightEditInvalidatesPreciseIntervalAndMatchesFull(t *testing.T) {
	loaded := loadSample(t, model.FormatWeightCast)
	replay := loaded.Replay
	oldGraph := graph.Build(replay)

	dbPath, cleanup, err := checkpoint.MustTempDB()
	if err != nil {
		t.Fatal(err)
	}
	defer cleanup()
	store, err := checkpoint.Open(dbPath)
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()

	first, err := graph.RunIncremental(context.Background(), replay, store)
	if err != nil {
		t.Fatal(err)
	}
	if len(first.CacheMissRounds) != replay.Rounds {
		t.Fatalf("initial misses = %v", first.CacheMissRounds)
	}

	replay.Edits = append(replay.Edits, model.Edit{
		Type: "weight", Round: 2, UnitID: "beta", Value: 10,
	})
	newGraph := graph.Build(replay)
	plan := graph.PlanInvalidation(oldGraph, newGraph)
	if len(plan.InvalidRounds) != 2 || plan.InvalidRounds[0] != 2 || plan.InvalidRounds[1] != 3 {
		t.Fatalf("invalid rounds = %+v", plan.InvalidRounds)
	}
	if len(plan.ReusedRounds) != 1 || plan.ReusedRounds[0] != 1 {
		t.Fatalf("reused rounds = %v", plan.ReusedRounds)
	}
	if len(plan.Intervals) != 1 || plan.Intervals[0] != (graph.Interval{From: 2, To: 3}) {
		t.Fatalf("intervals = %+v", plan.Intervals)
	}

	if err := store.Close(); err != nil {
		t.Fatal(err)
	}
	store, err = checkpoint.Open(dbPath)
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()

	inc, err := graph.RunIncremental(context.Background(), replay, store)
	if err != nil {
		t.Fatal(err)
	}
	if len(inc.CacheHitRounds) != 1 || inc.CacheHitRounds[0] != 1 {
		t.Fatalf("persistent hits = %v", inc.CacheHitRounds)
	}
	if len(inc.CacheMissRounds) != 2 || inc.CacheMissRounds[0] != 2 {
		t.Fatalf("incremental misses = %v", inc.CacheMissRounds)
	}
	full, err := engine.Run(context.Background(), replay)
	if err != nil {
		t.Fatal(err)
	}
	if err := diff.Snapshots(full.Snapshots, inc.Snapshots); err != nil {
		t.Fatalf("incremental/full E_DIFF: %v", err)
	}
}

func TestReorderEditGraphPropagation(t *testing.T) {
	loaded := loadSample(t, model.FormatWeightCast)
	oldGraph := graph.Build(loaded.Replay)
	loaded.Replay.Edits = append(loaded.Replay.Edits, model.Edit{
		Type: "reorder", Round: 2, Order: []string{"beta", "alpha", "red"},
	})
	plan := graph.PlanInvalidation(oldGraph, graph.Build(loaded.Replay))
	if got := plan.InvalidRounds; len(got) != 2 || got[0] != 2 || got[1] != 3 {
		t.Fatalf("reorder invalidation = %v report=%+v", got, plan)
	}
	if plan.TriggerNode != "edit:reorder@2" {
		t.Fatalf("trigger = %q", plan.TriggerNode)
	}
}
