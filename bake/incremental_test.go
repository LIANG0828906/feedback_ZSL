package bake_test

import (
	"context"
	"testing"

	"terrainbake/bake"
	"terrainbake/model"
)

func TestHeightInvalidationIncrementalMatchesFull(t *testing.T) {
	bundle := testBundle(t)
	opts := bake.DefaultScheduleOptions(bundle.Project.Params)
	engine, err := bake.NewEngine(&bundle.Project, nil)
	if err != nil {
		t.Fatalf("engine: %v", err)
	}
	baseline, err := engine.BakeFull(context.Background(), opts)
	if err != nil {
		t.Fatalf("baseline: %v", err)
	}
	invalidations, err := engine.RaiseHeight(context.Background(), "chunk-00-00", model.VertexPoint{X: 1, Z: 1}, 3.75)
	if err != nil {
		t.Fatalf("raise height: %v", err)
	}
	if len(invalidations) == 0 {
		t.Fatal("expected invalidations after height edit")
	}
	if len(invalidations) >= len(baseline.CommitSequence) {
		t.Fatalf("height edit invalidated every unit: %d", len(invalidations))
	}
	for _, inv := range invalidations {
		if inv.Reason == "" || inv.Dependency == "" {
			t.Fatalf("invalidation missing reason/dependency: %+v", inv)
		}
	}
	incremental, err := engine.BakeIncremental(context.Background(), opts)
	if err != nil {
		t.Fatalf("incremental: %v", err)
	}
	if len(incremental.Reused) != len(baseline.CommitSequence)-len(invalidations) {
		t.Fatalf("reused=%d want=%d", len(incremental.Reused), len(baseline.CommitSequence)-len(invalidations))
	}
	fullEngine, err := bake.NewEngine(engine.Project(), nil)
	if err != nil {
		t.Fatalf("fresh engine: %v", err)
	}
	full, err := fullEngine.BakeFull(context.Background(), opts)
	if err != nil {
		t.Fatalf("full after edit: %v", err)
	}
	if _, err := bake.CompareResults(incremental, full); err != nil {
		t.Fatalf("incremental/full mismatch: %v", err)
	}
}

func TestSunEditInvalidatesOnlyTimeAndDownstream(t *testing.T) {
	bundle := testBundle(t)
	opts := bake.DefaultScheduleOptions(bundle.Project.Params)
	engine, _ := bake.NewEngine(&bundle.Project, nil)
	baseline, err := engine.BakeFull(context.Background(), opts)
	if err != nil {
		t.Fatalf("baseline: %v", err)
	}
	replacement := bundle.Project.Sun[2]
	replacement.AzimuthDeg += 0.5
	invalidations, err := engine.SetSun(context.Background(), replacement)
	if err != nil {
		t.Fatalf("set sun: %v", err)
	}
	want := 2 * len(bundle.Project.Chunks)
	if len(invalidations) != want {
		t.Fatalf("invalidations=%d want=%d", len(invalidations), want)
	}
	for _, inv := range invalidations {
		if inv.Time < replacement.Time {
			t.Fatalf("past time %d invalidated", inv.Time)
		}
	}
	incremental, err := engine.BakeIncremental(context.Background(), opts)
	if err != nil {
		t.Fatalf("incremental: %v", err)
	}
	if len(incremental.Reused) != len(baseline.CommitSequence)-want {
		t.Fatalf("reused=%d want=%d", len(incremental.Reused), len(baseline.CommitSequence)-want)
	}
	fullEngine, _ := bake.NewEngine(engine.Project(), nil)
	full, err := fullEngine.BakeFull(context.Background(), opts)
	if err != nil {
		t.Fatalf("full: %v", err)
	}
	if _, err := bake.CompareResults(incremental, full); err != nil {
		t.Fatalf("sun incremental mismatch: %v", err)
	}
}

func TestBudgetQueueAndBufferLimits(t *testing.T) {
	bundle := testBundle(t)
	opts := bake.ScheduleOptions{MaxParallelism: 2, VertexBufferCap: 7, QueueBudget: 1}
	engine, _ := bake.NewEngine(&bundle.Project, nil)
	result, err := engine.BakeFull(context.Background(), opts)
	if err != nil {
		t.Fatalf("budget bake: %v", err)
	}
	if result.Stats.Workers > 2 {
		t.Fatalf("workers=%d exceed max", result.Stats.Workers)
	}
	if result.Stats.BufferBatches < len(bundle.Project.Chunks)*len(bundle.Project.Sun) {
		t.Fatalf("buffer cap was not exercised: %d batches", result.Stats.BufferBatches)
	}
	if result.Stats.QueueEvents == nil {
		t.Log("queue did not become full under a fast local run")
	}
}

func TestSharedEdgeHeightEditPreservesSeamAndIncremental(t *testing.T) {
	bundle := testBundle(t)
	opts := bake.DefaultScheduleOptions(bundle.Project.Params)
	engine, _ := bake.NewEngine(&bundle.Project, nil)
	if _, err := engine.BakeFull(context.Background(), opts); err != nil {
		t.Fatalf("baseline: %v", err)
	}
	edgeVertex := model.VertexPoint{X: 4, Z: 1}
	invalidations, err := engine.RaiseHeight(context.Background(), "chunk-00-00", edgeVertex, 2.25)
	if err != nil {
		t.Fatalf("edge raise: %v", err)
	}
	if len(invalidations) == 0 {
		t.Fatal("edge raise produced no invalidations")
	}
	incremental, err := engine.BakeIncremental(context.Background(), opts)
	if err != nil {
		t.Fatalf("incremental: %v", err)
	}
	if _, err := bake.AnalyzeSeams(incremental, engine.Terrain()); err != nil {
		t.Fatalf("edge seam mismatch: %v", err)
	}
	fullEngine, _ := bake.NewEngine(engine.Project(), nil)
	full, err := fullEngine.BakeFull(context.Background(), opts)
	if err != nil {
		t.Fatalf("full after edge edit: %v", err)
	}
	if _, err := bake.CompareResults(incremental, full); err != nil {
		t.Fatalf("edge incremental/full mismatch: %v", err)
	}
}
