package parallel_test

import (
	"testing"
	"time"

	"terrain-occlusion/internal/model"
	"terrain-occlusion/internal/parallel"
)

func TestSchedulerBoundsParallelismAndRecordsDepthWait(t *testing.T) {
	s := &parallel.Scheduler{MaxWorkers: 1, BufferLimit: 4}
	started := make(chan int, 2)
	release := make(chan struct{})
	tasks := []parallel.Task{
		{ChunkID: 1, DepthEntries: 2, Run: func(_ []float64) model.ChunkVerdict {
			started <- 1
			<-release
			return model.ChunkVerdict{ChunkID: 1, Final: model.VerdictVisible}
		}},
		{ChunkID: 2, DepthEntries: 2, Run: func(_ []float64) model.ChunkVerdict {
			started <- 2
			<-release
			return model.ChunkVerdict{ChunkID: 2, Final: model.VerdictOccluded}
		}},
	}

	done := make(chan []model.ChunkVerdict, 1)
	go func() { done <- s.Run(3, tasks) }()
	firstID := <-started

	deadline := time.Now().Add(2 * time.Second)
	sawDepthWait := false
	for time.Now().Before(deadline) {
		for _, w := range s.DrainWaits() {
			if w.Reason == "depth_buffers_exhausted" && w.SampleIndex == 3 && w.ChunkID != firstID {
				sawDepthWait = true
			}
		}
		if sawDepthWait {
			break
		}
		time.Sleep(time.Millisecond)
	}
	close(release)
	results := <-done
	if !sawDepthWait {
		t.Fatal("expected depth_buffers_exhausted wait record")
	}
	if s.MaxObservedParallelism() > 1 {
		t.Fatalf("parallelism exceeded bound: %d", s.MaxObservedParallelism())
	}
	if len(results) != 2 || results[1].ChunkID != 2 {
		t.Fatalf("results must preserve task order: %+v", results)
	}
}

func TestSchedulerRejectsTaskLargerThanThreadBuffer(t *testing.T) {
	s := &parallel.Scheduler{MaxWorkers: 2, BufferLimit: 4}
	results := s.Run(0, []parallel.Task{{ChunkID: 9, DepthEntries: 8, Run: func(_ []float64) model.ChunkVerdict {
		return model.ChunkVerdict{ChunkID: 9, Final: model.VerdictVisible}
	}}})
	if results[0].Final != model.VerdictUncertain {
		t.Fatalf("oversized depth task must degrade to uncertain, got %s", results[0].Final)
	}
	waits := s.DrainWaits()
	if len(waits) != 1 || waits[0].Reason != "depth_buffer_limit_exceeded" {
		t.Fatalf("expected limit record, got %+v", waits)
	}
}
