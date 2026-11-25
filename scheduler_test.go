package iconmask

import (
	"context"
	"sync/atomic"
	"testing"
	"time"
)

func TestParallelSchedulerBoundsAndQueueWait(t *testing.T) {
	tasks := []ParallelTask{
		{Index: 0, Block: Coord{X: 0}, PixelCount: 16},
		{Index: 1, Block: Coord{X: 1}, PixelCount: 16},
		{Index: 2, Block: Coord{X: 2}, PixelCount: 16},
		{Index: 3, Block: Coord{X: 3}, PixelCount: 16},
	}
	var running int32
	var peak int32
	var completed int32
	report, err := RunParallel(context.Background(), tasks, ParallelConfig{
		MaxWorkers:        2,
		PixelBufferBudget: 64,
		QueueBudget:       0,
	}, func(ctx context.Context, task ParallelTask, buffer *PixelBuffer) error {
		current := atomic.AddInt32(&running, 1)
		for {
			old := atomic.LoadInt32(&peak)
			if current <= old || atomic.CompareAndSwapInt32(&peak, old, current) {
				break
			}
		}
		if len(buffer.Data) != task.PixelCount || buffer.Cap != 64 {
			t.Errorf("unexpected worker buffer: len=%d cap=%d", len(buffer.Data), buffer.Cap)
		}
		time.Sleep(8 * time.Millisecond)
		atomic.AddInt32(&running, -1)
		atomic.AddInt32(&completed, 1)
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if peak > 2 || report.PeakInFlight > 2 {
		t.Fatalf("parallelism exceeded max workers: peak=%d report=%d", peak, report.PeakInFlight)
	}
	if completed != int32(len(tasks)) {
		t.Fatalf("not all tasks completed: %d", completed)
	}
	if len(report.WaitEvents) == 0 {
		t.Fatal("unbuffered queue must record queue wait events")
	}
	for _, event := range report.WaitEvents {
		if event.Reason != "queue_budget" || event.Duration <= 0 {
			t.Fatalf("invalid wait event: %+v", event)
		}
	}
}

func TestParallelPixelBufferBudget(t *testing.T) {
	_, err := RunParallel(context.Background(), []ParallelTask{{Index: 0, PixelCount: 100}}, ParallelConfig{
		MaxWorkers:        1,
		PixelBufferBudget: 10,
		QueueBudget:       1,
	}, func(ctx context.Context, task ParallelTask, buffer *PixelBuffer) error {
		return nil
	})
	domainErr := assertCode(t, err, CodeParameter)
	if domainErr.Field != "pixelBufferBudget" {
		t.Fatalf("unexpected pixel budget error: %+v", domainErr)
	}
}
