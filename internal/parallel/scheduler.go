package parallel

import (
	"sync"
	"sync/atomic"

	"terrain-occlusion/internal/model"
)

type Task struct {
	ChunkID      int
	DepthEntries int
	Run          func(depthBuffer []float64) model.ChunkVerdict
}

type Scheduler struct {
	MaxWorkers  int
	BufferLimit int

	mu        sync.Mutex
	Waits     []model.ResourceWait
	maxActive int32
	active    int32
}

func (s *Scheduler) Run(sampleIndex int, tasks []Task) []model.ChunkVerdict {
	workers := s.MaxWorkers
	if workers < 1 {
		workers = 1
	}
	limit := s.BufferLimit
	if limit < 1 {
		limit = 1
	}

	workerSlots := make(chan struct{}, workers)
	bufferPool := make(chan []float64, workers)
	for i := 0; i < workers; i++ {
		bufferPool <- make([]float64, 0, limit)
	}

	results := make([]model.ChunkVerdict, len(tasks))
	var wg sync.WaitGroup
	for i, task := range tasks {
		if task.DepthEntries > limit {
			v := model.ChunkVerdict{ChunkID: task.ChunkID, Coarse: "resource_rejected", Fine: model.VerdictUncertain, Final: model.VerdictUncertain}
			v.LowerBound, v.UpperBound = -1, 1
			results[i] = v
			s.recordWait(sampleIndex, task.ChunkID, "depth_buffer_limit_exceeded")
			continue
		}

		wg.Add(1)
		go func(i int, task Task) {
			defer wg.Done()

			var buffer []float64
			select {
			case buffer = <-bufferPool:
			default:
				s.recordWait(sampleIndex, task.ChunkID, "depth_buffers_exhausted")
				buffer = <-bufferPool
			}

			select {
			case workerSlots <- struct{}{}:
			default:
				s.recordWait(sampleIndex, task.ChunkID, "worker_pool_exhausted")
				workerSlots <- struct{}{}
			}

			active := atomic.AddInt32(&s.active, 1)
			for {
				max := atomic.LoadInt32(&s.maxActive)
				if active <= max || atomic.CompareAndSwapInt32(&s.maxActive, max, active) {
					break
				}
			}

			buffer = buffer[:0]
			if cap(buffer) < task.DepthEntries {
				buffer = make([]float64, 0, task.DepthEntries)
			}
			for j := 0; j < task.DepthEntries; j++ {
				buffer = append(buffer, 0)
			}
			results[i] = task.Run(buffer)

			atomic.AddInt32(&s.active, -1)
			<-workerSlots
			bufferPool <- buffer[:0]
		}(i, task)
	}
	wg.Wait()
	return results
}

func (s *Scheduler) recordWait(sample, chunk int, reason string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.Waits = append(s.Waits, model.ResourceWait{SampleIndex: sample, ChunkID: chunk, Reason: reason})
}

func (s *Scheduler) DrainWaits() []model.ResourceWait {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := append([]model.ResourceWait(nil), s.Waits...)
	s.Waits = nil
	return out
}

func (s *Scheduler) MaxObservedParallelism() int {
	return int(atomic.LoadInt32(&s.maxActive))
}
