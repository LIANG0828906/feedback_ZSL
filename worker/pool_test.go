package worker

import (
	"context"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

func TestPoolOrderingAndConcurrencyLimit(t *testing.T) {
	pool, err := New(3, 2)
	if err != nil {
		t.Fatal(err)
	}
	defer pool.Close()
	var active int32
	var maximum int32
	jobs := make([]Job, 12)
	for i := 0; i < len(jobs); i++ {
		index := i
		jobs[i] = Job{
			ID: i,
			Fn: func(ctx context.Context) (any, error) {
				current := atomic.AddInt32(&active, 1)
				for {
					old := atomic.LoadInt32(&maximum)
					if current <= old || atomic.CompareAndSwapInt32(&maximum, old, current) {
						break
					}
				}
				time.Sleep(time.Duration((len(jobs)-index)%4) * time.Millisecond)
				atomic.AddInt32(&active, -1)
				return index, nil
			},
		}
	}
	results, err := pool.Run(context.Background(), jobs)
	if err != nil {
		t.Fatal(err)
	}
	for i, result := range results {
		if result.ID != i || result.Err != nil {
			t.Fatalf("result %d: %+v", i, result)
		}
		if result.Value.(int) != i {
			t.Fatalf("out-of-order result at %d: %v", i, result.Value)
		}
	}
	if maximum > int32(pool.Workers()) {
		t.Fatalf("max concurrency %d exceeds worker limit %d", maximum, pool.Workers())
	}
}

func TestPoolRecoversPanics(t *testing.T) {
	pool, err := New(1, 1)
	if err != nil {
		t.Fatal(err)
	}
	defer pool.Close()
	results, err := pool.Run(context.Background(), []Job{{
		ID: 0,
		Fn: func(context.Context) (any, error) {
			panic("boom")
		},
	}})
	if err != nil {
		t.Fatal(err)
	}
	if results[0].Err == nil {
		t.Fatal("expected panic to be reported as an error")
	}
}

func TestPoolRejectsInvalidBounds(t *testing.T) {
	if _, err := New(0, 1); err == nil {
		t.Fatal("expected zero worker error")
	}
	if _, err := New(1, 0); err == nil {
		t.Fatal("expected zero buffer error")
	}
}

func TestFullWorkerBufferBlocksInsteadOfDropping(t *testing.T) {
	pool, err := New(1, 1)
	if err != nil {
		t.Fatal(err)
	}
	defer pool.Close()
	release := make(chan struct{})
	var started int32
	var mu sync.Mutex
	completed := []int{}
	job := func(id int, block bool) Job {
		return Job{
			ID: id,
			Fn: func(context.Context) (any, error) {
				atomic.AddInt32(&started, 1)
				if block {
					<-release
				}
				mu.Lock()
				completed = append(completed, id)
				mu.Unlock()
				return id, nil
			},
		}
	}
	jobs := []Job{job(0, true), job(1, false), job(2, false)}
	type runResult struct {
		results []Result
		err     error
	}
	done := make(chan runResult, 1)
	go func() {
		results, err := pool.Run(context.Background(), jobs)
		done <- runResult{results: results, err: err}
	}()
	time.Sleep(60 * time.Millisecond)
	if got := atomic.LoadInt32(&started); got != 1 {
		t.Fatalf("expected producer to block with one active job, got %d active starts", got)
	}
	close(release)
	select {
	case result := <-done:
		if result.err != nil {
			t.Fatal(result.err)
		}
		if len(result.results) != 3 {
			t.Fatalf("lost jobs: %d results", len(result.results))
		}
	case <-time.After(2 * time.Second):
		t.Fatal("pool did not drain after buffer release")
	}
	if len(completed) != 3 {
		t.Fatalf("completed %d jobs, want 3", len(completed))
	}
}
