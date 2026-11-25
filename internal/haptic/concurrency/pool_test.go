package concurrency

import (
	"sync/atomic"
	"testing"
	"time"
)

func TestSampleBudgetBlocksUntilRelease(t *testing.T) {
	budget := NewSampleBudget(8)
	budget.Acquire(8)
	acquired := make(chan struct{})
	go func() {
		budget.Acquire(4)
		close(acquired)
	}()
	select {
	case <-acquired:
		t.Fatal("acquire exceeded per-thread buffer without blocking")
	case <-time.After(30 * time.Millisecond):
	}
	if used := budget.Used(); used != 8 {
		t.Fatalf("used=%d, want 8", used)
	}
	budget.Release(8)
	select {
	case <-acquired:
	case <-time.After(time.Second):
		t.Fatal("blocked acquire did not resume after release")
	}
}

func TestMapOrderedPreservesResultsAndBoundsWorkers(t *testing.T) {
	var active, maxActive int32
	jobs := make([]Job, 12)
	for i := range jobs {
		i := i
		jobs[i] = Job{Samples: 1, Fn: func() (any, error) {
			cur := atomic.AddInt32(&active, 1)
			for {
				old := atomic.LoadInt32(&maxActive)
				if cur <= old || atomic.CompareAndSwapInt32(&maxActive, old, cur) {
					break
				}
			}
			time.Sleep(5 * time.Millisecond)
			atomic.AddInt32(&active, -1)
			return i * i, nil
		}}
	}
	errs, err := MapOrdered(jobs, 2, 4, func(index int, value any) error {
		if value.(int) != index*index {
			t.Fatalf("result %d = %v", index, value)
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	for _, jobErr := range errs {
		if jobErr != nil {
			t.Fatal(jobErr)
		}
	}
	if maxActive > 2 {
		t.Fatalf("max parallelism=%d, want <=2", maxActive)
	}
}
