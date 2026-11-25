package replaybench_test

import (
	"context"
	"sync"
	"testing"
	"time"

	"replaybench/internal/scheduler"
)

func TestBoundedPoolConcurrencyBudgetAndWaits(t *testing.T) {
	pool := scheduler.New(scheduler.Config{MaxWorkers: 4, WorkerBuffer: 1, QueueBudget: 8})
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	var wg sync.WaitGroup
	errs := make(chan error, 32)
	for i := 0; i < 32; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			err := pool.Submit(ctx, "cpu-task", func(ctx context.Context) error {
				select {
				case <-time.After(25 * time.Millisecond):
					return nil
				case <-ctx.Done():
					return ctx.Err()
				}
			})
			if err != nil {
				errs <- err
			}
		}(i)
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		t.Fatal(err)
	}
	stats := pool.Stats()
	if stats.MaxConcurrency > 4 {
		t.Fatalf("max concurrency %d exceeds 4", stats.MaxConcurrency)
	}
	if stats.MaxConcurrency != 4 {
		t.Fatalf("max concurrency = %d want 4", stats.MaxConcurrency)
	}
	if stats.WaitCount == 0 {
		t.Fatal("expected queue budget wait records")
	}
	foundBudgetWait := false
	for _, w := range stats.Waits {
		if w.Reason == "queue_budget_exhausted" && w.WaitedMS >= 0 {
			foundBudgetWait = true
		}
	}
	if !foundBudgetWait {
		t.Fatalf("missing budget wait: %+v", stats.Waits)
	}
	shutdownCtx, cancelShutdown := context.WithTimeout(context.Background(), time.Second)
	defer cancelShutdown()
	if err := pool.Shutdown(shutdownCtx); err != nil {
		t.Fatal(err)
	}
}

func TestPoolCancelWhileQueued(t *testing.T) {
	pool := scheduler.New(scheduler.Config{MaxWorkers: 1, WorkerBuffer: 0, QueueBudget: 1})
	release := make(chan struct{})
	started := make(chan struct{}, 1)
	go func() {
		_ = pool.Submit(context.Background(), "blocking", func(ctx context.Context) error {
			started <- struct{}{}
			<-release
			return nil
		})
	}()
	<-started

	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	err := pool.Submit(ctx, "canceled", func(context.Context) error { return nil })
	if err == nil {
		t.Fatal("expected canceled submit error")
	}
	close(release)
	shutdownCtx, cancelShutdown := context.WithTimeout(context.Background(), time.Second)
	defer cancelShutdown()
	_ = pool.Shutdown(shutdownCtx)
}
