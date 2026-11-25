package engine_test

import (
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"replayaudit/internal/engine"
)

func TestPoolNoLossAndParallelBound(t *testing.T) {
	pool := engine.NewPool(3, 4)
	var counter int64
	const n = 300
	for i := 0; i < n; i++ {
		pool.Submit("task", func() {
			atomic.AddInt64(&counter, 1)
			time.Sleep(200 * time.Microsecond)
		})
	}
	pool.Close()
	if counter != n {
		t.Fatalf("executed %d/%d tasks", counter, n)
	}
	if pool.Completed() != n {
		t.Fatalf("completed metric %d", pool.Completed())
	}
	if pool.PeakParallel() > 3 {
		t.Fatalf("peak %d exceeds max parallelism 3", pool.PeakParallel())
	}
	if pool.PeakParallel() < 2 {
		t.Fatalf("expected real parallel execution, peak=%d", pool.PeakParallel())
	}
}

func TestPoolBackpressureRecordedAndBlocks(t *testing.T) {
	pool := engine.NewPool(1, 1)
	gate := make(chan struct{})
	ran := make(chan struct{}, 1)
	pool.Submit("blocker", func() {
		ran <- struct{}{}
		<-gate
	})
	<-ran // worker is now occupied

	pool.Submit("buffered", func() {}) // occupies the single buffer slot

	done := make(chan struct{})
	go func() {
		pool.Submit("overflow", func() {})
		close(done)
	}()

	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) && len(pool.Blocks()) == 0 {
		time.Sleep(time.Millisecond)
	}
	blocks := pool.Blocks()
	if len(blocks) == 0 {
		close(gate)
		t.Fatalf("expected a recorded buffer-full block")
	}
	if blocks[0].Reason != "worker_buffer_full" || blocks[0].TaskID != "overflow" {
		t.Fatalf("unexpected block record: %+v", blocks[0])
	}

	select {
	case <-done:
		close(gate)
		t.Fatalf("overflow task must stay blocked until worker drains")
	default:
	}
	close(gate)
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatalf("blocked task did not drain after gate release")
	}
	pool.Close()
}

func TestPoolConcurrentSubmitSafety(t *testing.T) {
	pool := engine.NewPool(4, 8)
	var wg sync.WaitGroup
	var n int64
	for g := 0; g < 8; g++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for i := 0; i < 50; i++ {
				pool.Submit("x", func() { atomic.AddInt64(&n, 1) })
			}
		}()
	}
	wg.Wait()
	pool.Close()
	if n != 400 {
		t.Fatalf("ran %d", n)
	}
}
