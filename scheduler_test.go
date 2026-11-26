package maskqa

import (
	"testing"
	"time"
)

func TestSchedulerIsolatesPanic(t *testing.T) {
	scheduler := NewScheduler(SchedulerOptions{Parallelism: 2, BufferPixelCap: 128, QueueTileBudget: 2})
	stable := false
	results := scheduler.Run([]Job{
		{ID: "panic-job", Pixels: 1, Run: func([]byte) error { panic("injected tile failure") }},
		{ID: "stable-job", Pixels: 1, Run: func([]byte) error { stable = true; return nil }},
	})
	if results[0].Err == nil {
		t.Fatal("expected panic job error")
	}
	biz, ok := AsError(results[0].Err)
	if !ok || biz.Code != "E_WORKER_PANIC" {
		t.Fatalf("expected isolated E_WORKER_PANIC, got %+v", results[0].Err)
	}
	if results[1].Err != nil || !stable {
		t.Fatalf("stable job did not complete: %v", results[1].Err)
	}
}

func TestSchedulerBufferBudgetBlocksAndRecords(t *testing.T) {
	scheduler := NewScheduler(SchedulerOptions{Parallelism: 1, BufferPixelCap: 64, QueueTileBudget: 1})
	first, err := scheduler.acquireBuffer("first")
	if err != nil {
		t.Fatal(err)
	}
	done := make(chan struct{})
	go func() {
		buffer, acquireErr := scheduler.acquireBuffer("second")
		if acquireErr == nil {
			_ = buffer
		}
		close(done)
	}()
	select {
	case <-done:
		t.Fatal("second buffer acquisition should block while budget is exhausted")
	case <-time.After(50 * time.Millisecond):
	}
	scheduler.releaseBuffer(first)
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("blocked buffer acquisition did not resume")
	}
	metrics := scheduler.Snapshot()
	if metrics.BufferWaitCount == 0 {
		t.Fatalf("expected buffer exhaustion record, got %+v", metrics)
	}
	if metrics.Backpressure["pixel_buffer_budget_exhausted:second"] == 0 {
		t.Fatalf("missing backpressure reason: %+v", metrics.Backpressure)
	}
}
