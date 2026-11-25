package scheduler_test

import (
	"context"
	"errors"
	"fmt"
	"sync/atomic"
	"testing"
	"time"

	"wardrobe/scheduler"
)

func TestBoundedParallelismAndResultOrder(t *testing.T) {
	var active atomic.Int32
	var peak atomic.Int32
	tasks := make([]scheduler.Task[int], 0, 8)
	for index := 0; index < 8; index++ {
		taskIndex := index
		tasks = append(tasks, scheduler.Task[int]{
			ID:     fmt.Sprintf("T%d", index),
			Weight: 2,
			Run: func(ctx context.Context) (int, error) {
				current := active.Add(1)
				for {
					old := peak.Load()
					if current <= old || peak.CompareAndSwap(old, current) {
						break
					}
				}
				time.Sleep(5 * time.Millisecond)
				active.Add(-1)
				return taskIndex, nil
			},
		})
	}
	results, stats, err := scheduler.Run(context.Background(), scheduler.Config{
		MaxWorkers:               2,
		CandidateBufferPerWorker: 2,
		PendingQueueBudget:       2,
	}, tasks)
	if err != nil {
		t.Fatal(err)
	}
	if peak.Load() > 2 {
		t.Fatalf("parallelism exceeded limit: %d", peak.Load())
	}
	for index, result := range results {
		if result.Err != nil {
			t.Fatal(result.Err)
		}
		if result.Value != index {
			t.Fatalf("result order changed: results[%d]=%d", index, result.Value)
		}
	}
	if stats.PendingQueueBudget != 2 || stats.MaxPendingParts > 2 {
		t.Fatalf("unexpected stats: %+v", stats)
	}
	if len(stats.WaitEvents) == 0 {
		t.Fatalf("expected a bounded-queue wait event: %+v", stats)
	}
	for _, event := range stats.WaitEvents {
		if event.Reason != "pending_part_budget_exhausted" || event.Stage != "part_queue" {
			t.Fatalf("unexpected wait event: %+v", event)
		}
	}
}

func TestTaskFailureAndPanicAreIsolatedAndRetryable(t *testing.T) {
	tasks := make([]scheduler.Task[string], 0, 4)
	tasks = append(tasks,
		scheduler.Task[string]{ID: "ok-1", Run: func(context.Context) (string, error) { return "one", nil }},
		scheduler.Task[string]{ID: "bad", Run: func(context.Context) (string, error) { return "", errors.New("expected failure") }},
		scheduler.Task[string]{ID: "panic", Run: func(context.Context) (string, error) { panic("boom") }},
		scheduler.Task[string]{ID: "ok-2", Run: func(context.Context) (string, error) { return "two", nil }},
	)
	results, _, err := scheduler.Run(context.Background(), scheduler.Config{
		MaxWorkers:               2,
		CandidateBufferPerWorker: 2,
		PendingQueueBudget:       4,
	}, tasks)
	if err != nil {
		t.Fatal(err)
	}
	if results[0].Value != "one" || results[3].Value != "two" {
		t.Fatalf("healthy tasks were polluted: %+v", results)
	}
	if results[1].Err == nil || results[2].Err == nil {
		t.Fatalf("failed tasks were not reported: %+v", results)
	}
	retry, _, err := scheduler.Run(context.Background(), scheduler.Config{
		MaxWorkers:               1,
		CandidateBufferPerWorker: 1,
		PendingQueueBudget:       1,
	}, []scheduler.Task[string]{{
		ID: "bad-retry",
		Run: func(context.Context) (string, error) {
			return "recovered", nil
		},
	}})
	if err != nil {
		t.Fatal(err)
	}
	if len(retry) != 1 || retry[0].Value != "recovered" {
		t.Fatalf("retry failed: %+v", retry)
	}
}
