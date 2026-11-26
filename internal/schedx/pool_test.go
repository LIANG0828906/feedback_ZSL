package schedx

import (
	"context"
	"errors"
	"sync/atomic"
	"testing"
	"time"
)

func TestPoolLimitsParallelismAndIsolatesPanic(t *testing.T) {
	pool := &Pool{MaxParallel: 2, PathBuffer: 8, QueueBudget: 1}
	var current, max int32
	jobs := make([]Job, 0, 6)
	for i := 0; i < 6; i++ {
		i := i
		jobs = append(jobs, Job{ChunkID: "C", Run: func(workerID, cap int) error {
			if cap != 8 {
				t.Errorf("buffer cap = %d", cap)
			}
			cur := atomic.AddInt32(&current, 1)
			for {
				old := atomic.LoadInt32(&max)
				if cur <= old || atomic.CompareAndSwapInt32(&max, old, cur) {
					break
				}
			}
			time.Sleep(5 * time.Millisecond)
			atomic.AddInt32(&current, -1)
			if i == 3 {
				return errors.New("isolated")
			}
			if i == 4 {
				panic("boom")
			}
			return nil
		}})
	}
	errs, records := pool.Run(context.Background(), jobs)
	if len(errs) != 6 || len(records) != 6 {
		t.Fatalf("errors=%d records=%d", len(errs), len(records))
	}
	if max > 2 {
		t.Fatalf("parallelism max=%d", max)
	}
	if errs[3] == nil || errs[4] == nil {
		t.Fatalf("expected returned error and panic error, got %v %v", errs[3], errs[4])
	}
	if errs[0] != nil || errs[5] != nil {
		t.Fatalf("unexpected healthy job errors: %v %v", errs[0], errs[5])
	}
}

func TestPoolRecordsQueueBudgetBlock(t *testing.T) {
	pool := &Pool{MaxParallel: 1, PathBuffer: 4, QueueBudget: 1}
	started := make(chan struct{}, 1)
	release := make(chan struct{})
	jobs := []Job{
		{ChunkID: "blocking", Run: func(int, int) error {
			started <- struct{}{}
			<-release
			return nil
		}},
		{ChunkID: "queued-1", Run: func(int, int) error { return nil }},
		{ChunkID: "queued-2", Run: func(int, int) error { return nil }},
	}
	done := make(chan struct{})
	var records []struct {
		chunk   string
		blocked bool
		reason  string
	}
	go func() {
		_, qr := pool.Run(context.Background(), jobs)
		for _, q := range qr {
			records = append(records, struct {
				chunk   string
				blocked bool
				reason  string
			}{q.ChunkID, q.Blocked, q.Reason})
		}
		close(done)
	}()

	select {
	case <-started:
	case <-time.After(time.Second):
		t.Fatal("worker did not start")
	}
	time.Sleep(50 * time.Millisecond)
	close(release)
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("pool did not finish")
	}
	found := false
	for _, r := range records {
		if r.blocked && r.reason == "pending_chunk_queue_budget_exhausted" {
			found = true
		}
	}
	if !found {
		t.Fatalf("expected queue budget block record: %+v", records)
	}
}
