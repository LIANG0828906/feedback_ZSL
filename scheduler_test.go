package colortrace_test

import (
	"context"
	"errors"
	"testing"
	"time"

	ct "colortrace"
	"colortrace/sample"
)

func TestRealtimeCancelResumeAndFullPathsMatch(t *testing.T) {
	project, err := sample.Generate()
	if err != nil {
		t.Fatal(err)
	}
	sequential, err := ct.ExecuteSequential(project)
	if err != nil {
		t.Fatal(err)
	}
	budget := ct.Budget{MaxParallel: 2, PerWorkerSamples: 32, PendingTasks: 2}

	fullStore := ct.NewMemoryStore()
	fullScheduler := &ct.Scheduler{Store: fullStore, Budget: budget, Force: true}
	full, err := fullScheduler.Run(context.Background(), project)
	if err != nil {
		t.Fatal(err)
	}
	if len(full.CommitSequence) != 8 {
		t.Fatalf("expected 8 chunk commits, got %d", len(full.CommitSequence))
	}

	realtimeStore := ct.NewMemoryStore()
	realtimeScheduler := &ct.Scheduler{Store: realtimeStore, Budget: budget}
	realtime, err := realtimeScheduler.Run(context.Background(), project)
	if err != nil {
		t.Fatal(err)
	}
	if realtime.MaxObservedParallel < 1 || realtime.MaxObservedParallel > 2 {
		t.Fatalf("parallelism out of budget: %d", realtime.MaxObservedParallel)
	}
	if mismatch := ct.CompareResults(project, sequential, realtime.Result, realtime.CommitSequence, full.CommitSequence); mismatch != nil {
		t.Fatalf("path mismatch: %s", ct.DescribeMismatch(mismatch))
	}

	cancelStore := ct.NewMemoryStore()
	cancelScheduler := &ct.Scheduler{Store: cancelStore, Budget: ct.Budget{MaxParallel: 1, PerWorkerSamples: 16, PendingTasks: 1}, CancelAfterCommits: 1}
	cancelReport, err := cancelScheduler.Run(context.Background(), project)
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("expected context.Canceled, got %v", err)
	}
	if len(cancelReport.CommitSequence) != 1 {
		t.Fatalf("expected exactly one stable commit after cancel, got %d", len(cancelReport.CommitSequence))
	}
	resumeScheduler := &ct.Scheduler{Store: cancelStore, Budget: budget}
	resume, err := resumeScheduler.Run(context.Background(), project)
	if err != nil {
		t.Fatal(err)
	}
	if len(resume.CommitSequence) != 8 {
		t.Fatalf("resume sequence length=%d", len(resume.CommitSequence))
	}
	if mismatch := ct.CompareResults(project, sequential, resume.Result, resume.CommitSequence, full.CommitSequence); mismatch != nil {
		t.Fatalf("resume mismatch: %s", ct.DescribeMismatch(mismatch))
	}
	if len(resume.Skipped) != 1 {
		t.Fatalf("expected one reused chunk, got %v", resume.Skipped)
	}
}

func TestBudgetQueueWaitAndSampleBuffer(t *testing.T) {
	project, err := sample.Generate()
	if err != nil {
		t.Fatal(err)
	}
	store := ct.NewMemoryStore()
	entered := make(chan struct{}, 1)
	release := make(chan struct{})
	scheduler := &ct.Scheduler{
		Store:  store,
		Budget: ct.Budget{MaxParallel: 1, PerWorkerSamples: 1, PendingTasks: 1},
		Gate: func(_ context.Context, stage int, chunkID string) <-chan struct{} {
			if stage == 0 && chunkID == "c00x00" {
				select {
				case entered <- struct{}{}:
				default:
				}
				return release
			}
			return nil
		},
	}
	type runResult struct {
		report *ct.RunReport
		err    error
	}
	done := make(chan runResult, 1)
	go func() {
		report, err := scheduler.Run(context.Background(), project)
		done <- runResult{report, err}
	}()
	select {
	case <-entered:
	case <-time.After(2 * time.Second):
		t.Fatal("worker did not enter gated chunk")
	}
	time.Sleep(50 * time.Millisecond)
	close(release)
	select {
	case result := <-done:
		if result.err != nil {
			t.Fatal(result.err)
		}
		if len(result.report.QueueWaits) == 0 {
			t.Fatal("expected budget queue wait record")
		}
		if result.report.QueueWaits[0].Reason != "pending_queue_budget" {
			t.Fatalf("unexpected wait reason %q", result.report.QueueWaits[0].Reason)
		}
		if result.report.BufferFlushes == 0 {
			t.Fatal("expected bounded sample buffer flushes")
		}
		if result.report.MaxObservedParallel != 1 {
			t.Fatalf("max parallel=%d", result.report.MaxObservedParallel)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("scheduler did not finish after releasing gate")
	}
}

func TestChunkExceptionIsolatesAndRetries(t *testing.T) {
	project, err := sample.Generate()
	if err != nil {
		t.Fatal(err)
	}
	fullStore := ct.NewMemoryStore()
	full, err := (&ct.Scheduler{Store: fullStore, Budget: ct.Budget{MaxParallel: 2, PerWorkerSamples: 32, PendingTasks: 2}, Force: true}).Run(context.Background(), project)
	if err != nil {
		t.Fatal(err)
	}

	store := ct.NewMemoryStore()
	failing := &ct.Scheduler{
		Store: store, Budget: ct.Budget{MaxParallel: 1, PerWorkerSamples: 32, PendingTasks: 1},
		Inject: func(stage int, chunkID string) error {
			if stage == 0 && chunkID == "c01x00" {
				return &ct.Error{Code: ct.EDecode, Stage: stage, HasStage: true, Chunk: chunkID, Message: "injected truncated texture plane"}
			}
			return nil
		},
	}
	_, err = failing.Run(context.Background(), project)
	var ce *ct.Error
	if !errors.As(err, &ce) || ce.Code != ct.EDecode {
		t.Fatalf("expected injected E_DECODE, got %v", err)
	}
	records, err := store.Units(project.ProjectID)
	if err != nil {
		t.Fatal(err)
	}
	committed := 0
	failed := false
	for _, record := range records {
		if record.State == ct.StateCommitted {
			committed++
		}
		if record.State == ct.StateFailed && record.ChunkID == "c01x00" {
			failed = true
		}
	}
	if committed != 1 || !failed {
		t.Fatalf("expected one unaffected commit and failed target, committed=%d failed=%v", committed, failed)
	}

	retry := &ct.Scheduler{Store: store, Budget: ct.Budget{MaxParallel: 2, PerWorkerSamples: 32, PendingTasks: 2}}
	retryReport, err := retry.Run(context.Background(), project)
	if err != nil {
		t.Fatalf("retry should recover: %v", err)
	}
	if len(retryReport.CommitSequence) != 8 {
		t.Fatalf("expected canonical 8 commits after retry, got %d", len(retryReport.CommitSequence))
	}
	if mismatch := ct.CompareResults(project, full.Result, retryReport.Result, full.CommitSequence, retryReport.CommitSequence); mismatch != nil {
		t.Fatalf("retry/full mismatch: %s", ct.DescribeMismatch(mismatch))
	}
}
