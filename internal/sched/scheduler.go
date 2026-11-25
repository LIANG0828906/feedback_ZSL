package sched

import (
	"context"
	"fmt"
	"runtime"
	"sync"
	"sync/atomic"
	"time"
)

type Chunk struct {
	ID      int
	Samples int
	Run     func() error
}

type WaitEvent struct {
	Reason           string `json:"reason"`
	RequestedSamples int    `json:"requested_samples"`
	BudgetSamples    int64  `json:"budget_samples"`
	WaitedNanos      int64  `json:"waited_nanos"`
	WaitedMillis     int64  `json:"waited_millis"`
}

type Metrics struct {
	MaxParallelism         int         `json:"max_parallelism"`
	PerWorkerBufferSamples int         `json:"per_worker_buffer_samples"`
	QueueBudgetSamples     int64       `json:"queue_budget_samples"`
	SubmittedChunks        int64       `json:"submitted_chunks"`
	CompletedChunks        int64       `json:"completed_chunks"`
	PeakParallelWorkers    int64       `json:"peak_parallel_workers"`
	PeakPendingSamples     int64       `json:"peak_pending_samples"`
	PeakActiveSamples      int64       `json:"peak_active_samples"`
	QueueWaitCount         int64       `json:"queue_wait_count"`
	QueueWaitNanos         int64       `json:"queue_wait_nanos"`
	WaitEvents             []WaitEvent `json:"wait_events"`
}

type Scheduler struct {
	workers int
	bufCap  int
	budget  int64

	mu             sync.Mutex
	cond           *sync.Cond
	pendingSamples int64
	activeSamples  int64
	peakPending    int64
	peakActive     int64
	waitCount      int64
	waitNanos      int64
	waitEvents     []WaitEvent
	submitted      int64
	completed      int64
	peakWorkers    int64
}

func New(workers, perWorkerBufferSamples int, queueBudgetSamples int64) *Scheduler {
	if workers <= 0 {
		workers = runtime.GOMAXPROCS(0)
	}
	if workers > 8 {
		workers = 8
	}
	if perWorkerBufferSamples <= 0 {
		perWorkerBufferSamples = 4096
	}
	if queueBudgetSamples <= 0 {
		queueBudgetSamples = int64(workers * perWorkerBufferSamples)
	}
	return &Scheduler{workers: workers, bufCap: perWorkerBufferSamples, budget: queueBudgetSamples}
}

func (s *Scheduler) MaxParallelism() int { return s.workers }
func (s *Scheduler) BufferCap() int      { return s.bufCap }

func (s *Scheduler) Metrics() Metrics {
	s.mu.Lock()
	defer s.mu.Unlock()
	events := append([]WaitEvent(nil), s.waitEvents...)
	return Metrics{
		MaxParallelism: s.workers, PerWorkerBufferSamples: s.bufCap, QueueBudgetSamples: s.budget,
		SubmittedChunks: atomic.LoadInt64(&s.submitted), CompletedChunks: atomic.LoadInt64(&s.completed),
		PeakParallelWorkers: atomic.LoadInt64(&s.peakWorkers), PeakPendingSamples: s.peakPending,
		PeakActiveSamples: s.peakActive, QueueWaitCount: s.waitCount, QueueWaitNanos: s.waitNanos,
		WaitEvents: events,
	}
}

func (s *Scheduler) RunChunks(ctx context.Context, chunks []Chunk) error {
	if len(chunks) == 0 {
		return nil
	}
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()

	jobs := make(chan Chunk, len(chunks))
	var wg sync.WaitGroup
	var once sync.Once
	var firstErr error
	var activeWorkers int64
	setErr := func(err error) {
		if err == nil {
			return
		}
		once.Do(func() {
			firstErr = err
			cancel()
		})
	}

	for w := 0; w < s.workers; w++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for {
				select {
				case <-ctx.Done():
					return
				case chunk, ok := <-jobs:
					if !ok {
						return
					}
					s.mu.Lock()
					s.pendingSamples -= int64(chunk.Samples)
					s.activeSamples += int64(chunk.Samples)
					if s.activeSamples > s.peakActive {
						s.peakActive = s.activeSamples
					}
					if s.cond != nil {
						s.cond.Broadcast()
					}
					s.mu.Unlock()

					cur := atomic.AddInt64(&activeWorkers, 1)
					for {
						old := atomic.LoadInt64(&s.peakWorkers)
						if cur <= old || atomic.CompareAndSwapInt64(&s.peakWorkers, old, cur) {
							break
						}
					}
					func() {
						defer func() {
							if r := recover(); r != nil {
								setErr(fmt.Errorf("chunk %d panic: %v", chunk.ID, r))
							}
							atomic.AddInt64(&activeWorkers, -1)
							s.mu.Lock()
							s.activeSamples -= int64(chunk.Samples)
							s.mu.Unlock()
							atomic.AddInt64(&s.completed, 1)
						}()
						if chunk.Samples > s.bufCap {
							setErr(fmt.Errorf("chunk %d uses %d samples exceeding per-thread buffer %d", chunk.ID, chunk.Samples, s.bufCap))
							return
						}
						if err := chunk.Run(); err != nil {
							setErr(err)
						}
					}()
				}
			}
		}()
	}

	for _, chunk := range chunks {
		if chunk.Samples > s.bufCap {
			cancel()
			close(jobs)
			wg.Wait()
			return fmt.Errorf("chunk %d is %d samples; split below buffer cap %d", chunk.ID, chunk.Samples, s.bufCap)
		}
		select {
		case <-ctx.Done():
			close(jobs)
			wg.Wait()
			return firstErr
		default:
		}

		start := time.Now()
		waited := false
		s.mu.Lock()
		for s.pendingSamples+int64(chunk.Samples) > s.budget {
			waited = true
			s.waitCondLocked().Wait()
		}
		s.pendingSamples += int64(chunk.Samples)
		if s.pendingSamples > s.peakPending {
			s.peakPending = s.pendingSamples
		}
		if waited {
			d := time.Since(start)
			s.waitCount++
			s.waitNanos += int64(d)
			ev := WaitEvent{Reason: "pending_sample_budget_exhausted", RequestedSamples: chunk.Samples, BudgetSamples: s.budget, WaitedMillis: d.Milliseconds()}
			ev.WaitedNanos = int64(d)
			s.waitEvents = append(s.waitEvents, ev)
			if len(s.waitEvents) > 100 {
				s.waitEvents = s.waitEvents[len(s.waitEvents)-100:]
			}
		}
		s.mu.Unlock()
		atomic.AddInt64(&s.submitted, 1)

		select {
		case jobs <- chunk:
		case <-ctx.Done():
			s.mu.Lock()
			s.pendingSamples -= int64(chunk.Samples)
			s.mu.Unlock()
			close(jobs)
			wg.Wait()
			return firstErr
		}
	}
	close(jobs)
	wg.Wait()
	if firstErr != nil {
		return firstErr
	}
	m := s.Metrics()
	_ = m
	return nil
}

func (s *Scheduler) waitCondLocked() *sync.Cond {
	if s.cond == nil {
		s.cond = sync.NewCond(&s.mu)
	}
	return s.cond
}
