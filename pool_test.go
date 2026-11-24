package syncgame

import (
	"context"
	"strings"
	"sync"
	"testing"
	"time"
)

func TestWorkerPoolBlocksAndRecordsQueuePressure(t *testing.T) {
	pool := NewWorkerPool(PoolConfig{Workers: 1, QueuePerWorker: 1, StateBufferSize: 1})
	defer pool.Close()
	release := make(chan struct{})
	started := make(chan struct{})
	var wg sync.WaitGroup
	results := make(chan error, 3)
	submit := func(name string, block bool) {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_, err := pool.Submit(context.Background(), name, func() (any, error) {
				if block {
					close(started)
					<-release
				}
				return name, nil
			})
			results <- err
		}()
	}
	submit("blocker", true)
	<-started
	submit("queued", false)
	time.Sleep(20 * time.Millisecond)
	submit("pressure", false)
	deadline := time.Now().Add(2 * time.Second)
	for {
		stats := pool.Stats()
		if stats.QueueBlocks > 0 && stats.BlockReasons["queue_full:pressure"] > 0 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("queue pressure was not recorded: %+v", stats)
		}
		time.Sleep(5 * time.Millisecond)
	}
	close(release)
	wg.Wait()
	close(results)
	for err := range results {
		if err != nil {
			t.Fatal(err)
		}
	}
	if stats := pool.Stats(); stats.Completed != 3 || stats.Failed != 0 {
		t.Fatalf("unexpected pool completion stats: %+v", stats)
	}
}

func TestBoundedBufferBlocksWithoutDropping(t *testing.T) {
	buffer := NewBoundedBuffer[int](1)
	ctx := context.Background()
	if err := buffer.Push(ctx, 1, "test"); err != nil {
		t.Fatal(err)
	}
	done := make(chan error, 1)
	go func() {
		done <- buffer.Push(ctx, 2, "render_buffer_full")
	}()
	deadline := time.Now().Add(time.Second)
	for {
		blocks, _, reasons := buffer.Stats()
		if blocks > 0 && reasons["render_buffer_full"] > 0 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("buffer block not recorded: %v", reasons)
		}
		time.Sleep(5 * time.Millisecond)
	}
	first, err := buffer.Pop(ctx)
	if err != nil || first != 1 {
		t.Fatalf("pop = %d, %v", first, err)
	}
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	second, err := buffer.Pop(ctx)
	if err != nil || second != 2 {
		t.Fatalf("second pop = %d, %v", second, err)
	}
}

func TestWorkerPoolPanicBecomesError(t *testing.T) {
	pool := NewWorkerPool(PoolConfig{Workers: 1, QueuePerWorker: 1})
	defer pool.Close()
	_, err := pool.Submit(context.Background(), "panic", func() (any, error) {
		panic("boom")
	})
	if err == nil || !strings.Contains(err.Error(), "boom") {
		t.Fatalf("expected panic error, got %v", err)
	}
}
