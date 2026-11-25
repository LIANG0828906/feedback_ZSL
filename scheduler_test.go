package acousticfield

import (
	"context"
	"errors"
	"path/filepath"
	"sync/atomic"
	"testing"
	"time"
)

func TestSchedulerQueueBudgetAndFailureIsolation(t *testing.T) {
	s := NewParallelScheduler(2, 1)
	var ran atomic.Int64
	var jobs []WorkItem
	for i := 0; i < 12; i++ {
		id := i
		jobs = append(jobs, WorkItem{ChunkID: id, Run: func(ctx context.Context, workerID int) error {
			ran.Add(1)
			time.Sleep(5 * time.Millisecond)
			if id == 5 {
				return errors.New("injected chunk failure")
			}
			return nil
		}})
	}
	summary := s.Run(context.Background(), jobs)
	if ran.Load() != 12 {
		t.Fatalf("ran %d jobs", ran.Load())
	}
	if len(summary.Errors) != 1 || summary.Errors[0].ChunkID != 5 {
		t.Fatalf("failure isolation broke: %+v", summary.Errors)
	}
	if len(summary.Completed) != 11 {
		t.Fatalf("completed %d jobs", len(summary.Completed))
	}
	if len(summary.QueueEvents) == 0 {
		t.Fatal("queue budget exhaustion was not recorded")
	}
	for _, e := range summary.QueueEvents {
		if e.Reason == "" || e.WaitText == "" {
			t.Fatalf("incomplete queue event: %+v", e)
		}
	}
}

func TestSchedulerPanicIsolation(t *testing.T) {
	s := NewParallelScheduler(1, 1)
	summary := s.Run(context.Background(), []WorkItem{
		{ChunkID: 0, Run: func(context.Context, int) error { panic("boom") }},
		{ChunkID: 1, Run: func(context.Context, int) error { return nil }},
	})
	if len(summary.Errors) != 1 || !IsCode(summary.Errors[0].Err, CodeState) {
		t.Fatalf("panic not isolated: %+v", summary.Errors)
	}
	if len(summary.Completed) != 1 {
		t.Fatalf("second chunk did not complete")
	}
}

func TestParallelWorkerCountDoesNotChangeResults(t *testing.T) {
	sample, err := Generate(707)
	if err != nil {
		t.Fatal(err)
	}
	base := DefaultOptions()
	one := base
	one.MaxParallel = 1
	one.QueueBudget = 2
	four := base
	four.MaxParallel = 4
	four.QueueBudget = 13

	e1 := newTestEngine(t, sample, t.TempDir()+"/one.db", one)
	r1, err := e1.Rebuild(context.Background(), BuildOptions{})
	if err != nil {
		t.Fatal(err)
	}
	e4 := newTestEngine(t, sample, t.TempDir()+"/four.db", four)
	r4, err := e4.Rebuild(context.Background(), BuildOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if err := CompareRunReports(r1, r4); err != nil {
		t.Fatal(err)
	}
}

func TestSchedulingBudgetDoesNotInvalidateCheckpointIdentity(t *testing.T) {
	sample, err := Generate(717)
	if err != nil {
		t.Fatal(err)
	}
	dbPath := filepath.Join(t.TempDir(), "reuse.db")
	one := DefaultOptions()
	one.MaxParallel = 1
	one.QueueBudget = 1
	store1, err := OpenStore(dbPath)
	if err != nil {
		t.Fatal(err)
	}
	e1, err := NewEngine(sample.Level, &sample.Truth, one, store1)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := e1.Initialize(context.Background()); err != nil {
		t.Fatal(err)
	}
	if _, err := e1.Rebuild(context.Background(), BuildOptions{}); err != nil {
		t.Fatal(err)
	}
	if err := store1.Close(); err != nil {
		t.Fatal(err)
	}

	four := one
	four.MaxParallel = 4
	four.QueueBudget = 16
	store4, err := OpenStore(dbPath)
	if err != nil {
		t.Fatal(err)
	}
	defer store4.Close()
	e4, err := NewEngine(sample.Level, &sample.Truth, four, store4)
	if err != nil {
		t.Fatal(err)
	}
	restore, err := e4.Initialize(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(restore.Loaded) != len(sample.Level.Chunks) {
		t.Fatalf("restored %d chunks across scheduling-budget change", len(restore.Loaded))
	}
	run, err := e4.Rebuild(context.Background(), BuildOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if run.CacheHits != len(sample.Level.Chunks) {
		t.Fatalf("cache hits=%d, want all %d", run.CacheHits, len(sample.Level.Chunks))
	}
}
