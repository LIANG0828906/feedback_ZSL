package workpool_test

import (
	"context"
	"sync/atomic"
	"testing"
	"time"

	"lipsync.local/prototype/pkg/workpool"
)

func TestPoolBoundsAndRecordsBackpressure(t *testing.T) {
	var active int32
	var maximum int32
	var completed int32
	pool := workpool.Pool{Workers: 1, PerWorkerBuffer: 0, QueueBudget: 1}
	ids := []string{"0", "1", "2", "3", "4"}
	results, events, err := pool.Run(context.Background(), ids, func(ctx context.Context, index int, id string) error {
		now := atomic.AddInt32(&active, 1)
		for {
			old := atomic.LoadInt32(&maximum)
			if now <= old || atomic.CompareAndSwapInt32(&maximum, old, now) {
				break
			}
		}
		time.Sleep(5 * time.Millisecond)
		atomic.AddInt32(&active, -1)
		atomic.AddInt32(&completed, 1)
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	for i, result := range results {
		if result != nil {
			t.Fatalf("task %d failed: %v", i, result)
		}
	}
	if completed != int32(len(ids)) {
		t.Fatalf("tasks dropped: completed=%d", completed)
	}
	if maximum > 1 {
		t.Fatalf("parallelism exceeded worker limit: %d", maximum)
	}
	if len(events) == 0 {
		t.Fatal("expected a queue/worker backpressure event")
	}
	for _, event := range events {
		if event.Wait <= 0 || event.Reason == "" {
			t.Fatalf("incomplete backpressure evidence: %+v", event)
		}
	}
}
