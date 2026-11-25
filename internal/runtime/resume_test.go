package runtime_test

import (
	"path/filepath"
	"testing"

	"presentkernel/internal/checkpoint"
	"presentkernel/internal/engine"
	"presentkernel/internal/generator"
	"presentkernel/internal/runtime"
	"presentkernel/internal/scheduler"
)

func TestResumeCommitsContiguousPartitions(t *testing.T) {
	set, err := generator.GenerateSampleSet()
	if err != nil {
		t.Fatal(err)
	}
	db := filepath.Join(t.TempDir(), "checkpoint.sqlite")
	first, err := runtime.RunResume(set.Canonical, db, "run-a", 2, nil)
	if err != nil {
		t.Fatal(err)
	}
	if !first.Interrupted || len(first.Snapshot.Sequence) != 2 {
		t.Fatalf("expected interruption after 2, got interrupted=%v seq=%v", first.Interrupted, first.Snapshot.Sequence)
	}
	second, err := runtime.RunResume(set.Canonical, db, "run-a", -1, nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(second.Snapshot.Sequence) != len(runtime.Refs(set.Canonical)) {
		t.Fatalf("expected all partitions, got %v", second.Snapshot.Sequence)
	}
	store, err := checkpoint.Open(db)
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	status, err := store.Status("run-a")
	if err != nil {
		t.Fatal(err)
	}
	if status.RunRows != len(runtime.Refs(set.Canonical)) {
		t.Fatalf("checkpoint rows=%d want %d", status.RunRows, len(runtime.Refs(set.Canonical)))
	}
}

func TestThreePathsMatchAcrossEdits(t *testing.T) {
	set, err := generator.GenerateSampleSet()
	if err != nil {
		t.Fatal(err)
	}
	db := filepath.Join(t.TempDir(), "three-paths.sqlite")
	edits := []engine.Edit{
		{Kind: "char_weight", DialogueID: "D0", PartitionIndex: 0, CharIndex: 1, Value: 1300},
		{Kind: "segment_duration", DialogueID: "D1", PartitionIndex: 0, Value: 54},
		{Kind: "target_fps", Value: 30},
	}
	report, err := runtime.VerifyEditSequence(set.Canonical, edits, db, scheduler.Config{MaxParallel: 2, PerWorkerBuffer: 1, QueueBudget: 2})
	if err != nil {
		t.Fatal(err)
	}
	if !report.OK || report.FirstMismatch != nil {
		t.Fatalf("three-path mismatch: %+v", report.FirstMismatch)
	}
	if len(report.Steps) != 4 {
		t.Fatalf("steps=%d", len(report.Steps))
	}
}
