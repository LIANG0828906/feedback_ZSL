package grid

import (
	"context"
	"testing"
	"time"
)

func TestRayPoolParallelAndBoundedBuffers(t *testing.T) {
	pool := NewRayPool(PoolConfig{MaxParallelism: 2, RayBufferLimit: 2, RayDelay: 2 * time.Millisecond}, nil)
	const jobs = 6
	const raysPerJob = 6
	var tileJobs []TileJob
	for j := 0; j < jobs; j++ {
		j := j
		tileJobs = append(tileJobs, func(ctx context.Context, workerID int) error {
			for r := 0; r < raysPerJob; r++ {
				if err := pool.SubmitRay(ctx, workerID, Ray{Tile: TileID{Col: j, Row: workerID}, Index: r}); err != nil {
					return err
				}
			}
			pool.Flush(workerID)
			return nil
		})
	}
	if err := pool.RunJobs(context.Background(), tileJobs); err != nil {
		t.Fatal(err)
	}
	stats := pool.Stats()
	if stats.PeakParallel > 2 {
		t.Fatalf("peak parallel %d exceeds limit", stats.PeakParallel)
	}
	if stats.PeakParallel != 2 {
		t.Fatalf("peak parallel = %d, want 2", stats.PeakParallel)
	}
	if stats.Completed != jobs*raysPerJob {
		t.Fatalf("completed rays = %d want %d", stats.Completed, jobs*raysPerJob)
	}
	if len(stats.Waits) == 0 {
		t.Fatal("expected blocking wait records for saturated pool/buffers")
	}
	foundBufferWait := false
	for _, w := range stats.Waits {
		if w.Reason == "ray-buffer-exhausted" && w.WaitedNS > 0 {
			foundBufferWait = true
		}
	}
	if !foundBufferWait {
		t.Fatalf("no positive ray-buffer wait: %+v", stats.Waits)
	}
	pool.Shutdown()
}

func TestRayPoolReturnsEBlockForTile(t *testing.T) {
	pool := NewRayPool(PoolConfig{MaxParallelism: 1, RayBufferLimit: 1}, nil)
	defer pool.Shutdown()
	want := InjectBlockFault(DefaultCamera())
	err := pool.RunJobs(context.Background(), []TileJob{
		func(context.Context, int) error { return want },
	})
	ge, ok := AsError(err, CodeBlock)
	if !ok || ge.Tile == nil || want.Tile == nil || *ge.Tile != *want.Tile || ge.ObjectID != "OB01" {
		t.Fatalf("block error = %#v", err)
	}
}
