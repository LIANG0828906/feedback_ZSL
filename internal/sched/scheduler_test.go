package sched_test

import (
	"context"
	"sync/atomic"
	"testing"
	"time"

	"cadlab/internal/sched"
)

func TestSchedulerLimitsBudgetsAndRecordsQueues(t *testing.T) {
	s, err := sched.New(sched.Options{
		MaxParallel: 2, PerTaskPixelBytes: 64, PerTaskMeshBytes: 64,
		TotalPixelBytes: 64, TotalMeshBytes: 256,
	})
	if err != nil {
		t.Fatal(err)
	}
	var active, peak int32
	tasks := make([]sched.Task, 8)
	for i := range tasks {
		tasks[i] = sched.Task{
			ID: i, Label: "tile", PixelBytes: 64, MeshBytes: 64,
			Run: func(ctx context.Context) (any, error) {
				now := atomic.AddInt32(&active, 1)
				for {
					old := atomic.LoadInt32(&peak)
					if now <= old || atomic.CompareAndSwapInt32(&peak, old, now) {
						break
					}
				}
				time.Sleep(8 * time.Millisecond)
				atomic.AddInt32(&active, -1)
				return nil, nil
			},
		}
	}
	results, metrics, err := s.Run(context.Background(), tasks)
	if err != nil {
		t.Fatal(err)
	}
	if len(results) != len(tasks) || metrics.Completed != len(tasks) {
		t.Fatalf("lost tasks: results=%d completed=%d", len(results), metrics.Completed)
	}
	if peak > 2 || metrics.PeakParallel > 2 {
		t.Fatalf("parallel limit exceeded: peak=%d metric=%d", peak, metrics.PeakParallel)
	}
	if metrics.PeakPixelBytes > 64 || metrics.PeakMeshBytes > 256 {
		t.Fatalf("buffer budget exceeded: pixels=%d mesh=%d", metrics.PeakPixelBytes, metrics.PeakMeshBytes)
	}
	if len(metrics.Queue) != len(tasks) {
		t.Fatalf("queue records missing: %d", len(metrics.Queue))
	}
	var sawBudgetReason bool
	for _, q := range metrics.Queue {
		if q.Reason == "pixel-budget" || q.Reason == "mesh-budget" {
			sawBudgetReason = true
		}
	}
	if !sawBudgetReason {
		t.Fatalf("expected an observable budget queue reason: %+v", metrics.Queue)
	}
}

func TestSchedulerCancellationHasResultPerTask(t *testing.T) {
	s, _ := sched.New(sched.Options{
		MaxParallel: 1, PerTaskPixelBytes: 10, PerTaskMeshBytes: 10,
		TotalPixelBytes: 10, TotalMeshBytes: 10,
	})
	ctx, cancel := context.WithCancel(context.Background())
	tasks := []sched.Task{
		{ID: 0, PixelBytes: 10, MeshBytes: 10, Run: func(ctx context.Context) (any, error) {
			time.Sleep(5 * time.Millisecond)
			cancel()
			return "complete", nil
		}},
		{ID: 1, PixelBytes: 10, MeshBytes: 10, Run: func(ctx context.Context) (any, error) { return "unexpected", nil }},
	}
	results, metrics, err := s.Run(ctx, tasks)
	if err == nil {
		t.Fatal("expected cancellation error")
	}
	if len(results) != 2 || metrics.Cancelled == 0 {
		t.Fatalf("cancelled work dropped: results=%d metrics=%+v", len(results), metrics)
	}
}
