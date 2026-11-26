package pipeline

import (
	"context"
	"sync"
	"time"

	"iconflow/errs"
	"iconflow/model"
	"iconflow/state"
)

type job struct {
	coord  model.BlockCoord
	pixels int
	run    func() (*BlockResult, *JobError)
	done   chan JobResult
}

type JobResult struct {
	Block *BlockResult
	Error *JobError
}

type JobHandle struct {
	done chan JobResult
}

func (h *JobHandle) Wait(_ context.Context) JobResult {
	return <-h.done
}

type Scheduler struct {
	budget Budget
	queue  chan *job
	wg     sync.WaitGroup

	mu     sync.Mutex
	events []QueueEvent
	closed bool
}

func NewScheduler(budget Budget) *Scheduler {
	if budget.Workers <= 0 {
		budget.Workers = 1
	}
	if budget.MaxQueuedBlocks <= 0 {
		budget.MaxQueuedBlocks = budget.Workers
	}
	if budget.MaxPixelsPerThread <= 0 {
		budget.MaxPixelsPerThread = 4096
	}
	s := &Scheduler{
		budget: budget,
		queue:  make(chan *job, budget.MaxQueuedBlocks),
	}
	for i := 0; i < budget.Workers; i++ {
		s.wg.Add(1)
		go s.worker()
	}
	return s
}

func (s *Scheduler) worker() {
	defer s.wg.Done()
	for incoming := range s.queue {
		if incoming.pixels > s.budget.MaxPixelsPerThread {
			incoming.done <- JobResult{Error: &JobError{
				Coord: incoming.coord, Stage: state.StageGradient,
				Err: errs.New(errs.EBudget, incoming.coord.Key(),
					"block pixel buffer %d exceeds per-thread limit %d",
					incoming.pixels, s.budget.MaxPixelsPerThread),
			}}
			continue
		}
		block, jobErr := incoming.run()
		incoming.done <- JobResult{Block: block, Error: jobErr}
	}
}

func (s *Scheduler) Submit(coord model.BlockCoord, pixels int, run func() (*BlockResult, *JobError)) *JobHandle {
	handle := &JobHandle{done: make(chan JobResult, 1)}
	incoming := &job{coord: coord, pixels: pixels, run: run, done: handle.done}

	select {
	case s.queue <- incoming:
	default:
		start := time.Now()
		s.queue <- incoming
		waited := time.Since(start)
		event := QueueEvent{
			BlockKey: coord.Key(), Reason: "pending_block_queue_budget_exhausted",
			WaitedMS: waited.Milliseconds(), WaitedNS: waited.Nanoseconds(),
			At: time.Now().UTC(),
		}
		s.mu.Lock()
		s.events = append(s.events, event)
		s.mu.Unlock()
	}
	return handle
}

func (s *Scheduler) Events() []QueueEvent {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]QueueEvent(nil), s.events...)
}

func (s *Scheduler) Close() {
	s.mu.Lock()
	if s.closed {
		s.mu.Unlock()
		return
	}
	s.closed = true
	s.mu.Unlock()
	close(s.queue)
	s.wg.Wait()
}
