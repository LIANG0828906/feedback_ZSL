package audiofp_test

import (
	"context"
	"sync/atomic"
	"testing"
	"time"

	"audiofp"
)

func TestBufferSmallerThanFrameRejected(t *testing.T) {
	params := audiofp.DefaultParams()
	limits := audiofp.DefaultResourceLimits()
	limits.PerWorkerBufferSamples = params.FrameLength - 1
	_, err := audiofp.ExtractFrames(make([]float64, params.FrameLength*3), 16000, params, limits)
	if !audiofp.HasCode(err, audiofp.CodePara) {
		t.Fatalf("expected E_PARA buffer limit, got %v", err)
	}
	if de, _ := audiofp.AsError(err); de == nil || de.Field != "per_worker_buffer_samples" {
		t.Fatalf("unexpected field: %#v", de)
	}
}

func TestWorkerPoolDoesNotDropJobsAndBlocksWhenSaturated(t *testing.T) {
	ctx := context.Background()
	pool, err := audiofp.NewWorkerPool(audiofp.ResourceLimits{
		MaxWorkers: 1, PerWorkerBufferSamples: 128, MaxQueuedJobs: 1,
	})
	if err != nil {
		t.Fatal(err)
	}

	var handled int64
	for i := 0; i < 200; i++ {
		err := pool.Submit(ctx, audiofp.BufferJob{NeededSamples: 16, Handle: func(buf []float64) error {
			atomic.AddInt64(&handled, 1)
			return nil
		}})
		if err != nil {
			t.Fatal(err)
		}
	}
	if err := pool.Close(); err != nil {
		t.Fatal(err)
	}
	if handled != 200 {
		t.Fatalf("expected all jobs handled, got %d", handled)
	}

	started := make(chan struct{})
	release := make(chan struct{})
	pool2, err := audiofp.NewWorkerPool(audiofp.ResourceLimits{
		MaxWorkers: 1, PerWorkerBufferSamples: 128, MaxQueuedJobs: 1,
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := pool2.Submit(ctx, audiofp.BufferJob{NeededSamples: 8, Handle: func(buf []float64) error {
		close(started)
		<-release
		return nil
	}}); err != nil {
		t.Fatal(err)
	}
	<-started

	timeoutCtx, cancel := context.WithTimeout(ctx, 60*time.Millisecond)
	defer cancel()
	// One job occupies the worker, one fills the capacity-1 queue; the next
	// submit must apply backpressure.
	if err := pool2.Submit(ctx, audiofp.BufferJob{NeededSamples: 8, Handle: func([]float64) error { return nil }}); err != nil {
		t.Fatal(err)
	}
	err = pool2.Submit(timeoutCtx, audiofp.BufferJob{NeededSamples: 8, Handle: func([]float64) error { return nil }})
	if err == nil {
		t.Fatalf("expected saturated pool to block until context timeout")
	}
	close(release)
	if err := pool2.Close(); err != nil {
		t.Fatal(err)
	}
}
