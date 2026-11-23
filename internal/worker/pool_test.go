package worker_test

import (
	"context"
	"sync/atomic"
	"testing"

	"occlbench/internal/core"
	"occlbench/internal/worker"
)

func TestPoolParallelBounds(t *testing.T) {
	pool, err := worker.New(3, 64*64)
	if err != nil {
		t.Fatal(err)
	}
	if pool.MaxWorker() != 3 {
		t.Fatalf("max worker = %d", pool.MaxWorker())
	}
	var counter int64
	err = pool.Parallel(context.Background(), 100, func(i int) error {
		atomic.AddInt64(&counter, 1)
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if counter != 100 {
		t.Fatalf("only %d tasks ran", counter)
	}
}

func TestPoolRasterBufferCap(t *testing.T) {
	pool, _ := worker.New(2, 1024)
	if err := pool.CheckRasterBuffer(32); err != nil { // 1024 cells, at cap
		t.Fatalf("32x32 should be allowed: %v", err)
	}
	err := pool.CheckRasterBuffer(33) // 1089 > 1024
	if core.CodeOf(err) != core.EPara {
		t.Fatalf("want E_PARA got %v", err)
	}
}

func TestPoolCancellation(t *testing.T) {
	pool, _ := worker.New(2, 1024)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	var ran int64
	err := pool.Parallel(ctx, 10, func(i int) error {
		atomic.AddInt64(&ran, 1)
		return nil
	})
	if err == nil {
		t.Fatal("expected context cancellation error")
	}
}
