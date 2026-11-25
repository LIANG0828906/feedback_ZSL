package engine

import (
	"context"
	"fmt"
	"sync"

	"maskpreview/internal/core"
)

type Budget struct {
	MaxParallel      int   `json:"max_parallel"`
	MaxPerBlockBytes int64 `json:"max_per_block_bytes"`
	GlobalBytes      int64 `json:"global_bytes"`
}

type QueueEvent struct {
	BlockID int    `json:"block_id"`
	Label   string `json:"label"`
	Reason  string `json:"reason"`
}

type SchedulerStats struct {
	MaxActiveObserved int          `json:"max_active_observed"`
	QueuedEvents      []QueueEvent `json:"queued_events"`
	CompletedJobs     int          `json:"completed_jobs"`
}

type Scheduler struct {
	budget Budget
	mu     sync.Mutex
	cond   *sync.Cond

	active    int
	maxActive int
	globalUse int64
	blockUse  map[int]int64
	events    []QueueEvent
	completed int
}

type Job struct {
	BlockID int
	Run     func(context.Context) error
}

func NewScheduler(budget Budget) (*Scheduler, error) {
	if budget.MaxParallel <= 0 {
		return nil, core.ParamError("scheduler.new", "max_parallel", budget.MaxParallel, 1, 1024)
	}
	if budget.MaxPerBlockBytes <= 0 {
		return nil, core.ParamError("scheduler.new", "max_per_block_bytes", budget.MaxPerBlockBytes, 1, "unbounded")
	}
	if budget.GlobalBytes <= 0 {
		return nil, core.ParamError("scheduler.new", "global_bytes", budget.GlobalBytes, 1, "unbounded")
	}
	s := &Scheduler{
		budget:   budget,
		blockUse: map[int]int64{},
	}
	s.cond = sync.NewCond(&s.mu)
	return s, nil
}

// Run executes block jobs through a FIFO worker pool. Stage-level byte
// admission is performed by Acquire inside each job. FIFO dispatch guarantees
// the first block starts when MaxParallel == 1, which avoids commit-gate
// starvation while retaining bounded parallelism.
func (s *Scheduler) Run(ctx context.Context, jobs []Job) error {
	if len(jobs) == 0 {
		return nil
	}
	workers := s.budget.MaxParallel
	if workers > len(jobs) {
		workers = len(jobs)
	}
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	jobCh := make(chan Job)
	errCh := make(chan error, 1)
	var wg sync.WaitGroup
	for i := 0; i < workers; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for {
				select {
				case <-ctx.Done():
					return
				case job, ok := <-jobCh:
					if !ok {
						return
					}
					s.mu.Lock()
					s.active++
					if s.active > s.maxActive {
						s.maxActive = s.active
					}
					s.mu.Unlock()
					err := job.Run(ctx)
					s.mu.Lock()
					s.active--
					if err == nil {
						s.completed++
					}
					s.mu.Unlock()
					if err != nil {
						select {
						case errCh <- err:
						default:
						}
						cancel()
						return
					}
				}
			}
		}()
	}
	go func() {
		defer close(jobCh)
		for _, job := range jobs {
			select {
			case <-ctx.Done():
				return
			case jobCh <- job:
			}
		}
	}()
	wg.Wait()
	select {
	case err := <-errCh:
		return err
	default:
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	return nil
}

// Acquire reserves both global and per-block byte budget. If the request would
// exceed a limit, the caller blocks and the reason is recorded rather than the
// stage being dropped.
func (s *Scheduler) Acquire(ctx context.Context, blockID int, bytes int64, label string) (func(), error) {
	if bytes < 0 {
		return nil, core.ParamError("scheduler.acquire", "bytes", bytes, 0, "unbounded")
	}
	if bytes > s.budget.MaxPerBlockBytes {
		return nil, &core.Error{
			Code:    core.CodeResource,
			Op:      "scheduler.acquire",
			Field:   "max_per_block_bytes",
			Got:     bytes,
			Want:    s.budget.MaxPerBlockBytes,
			Message: "single stage exceeds per-block buffer limit",
		}
	}
	if bytes > s.budget.GlobalBytes {
		return nil, &core.Error{
			Code:    core.CodeResource,
			Op:      "scheduler.acquire",
			Field:   "global_bytes",
			Got:     bytes,
			Want:    s.budget.GlobalBytes,
			Message: "single stage exceeds global buffer budget",
		}
	}
	recorded := false
	stopWatch := context.AfterFunc(ctx, func() {
		s.mu.Lock()
		s.cond.Broadcast()
		s.mu.Unlock()
	})
	defer stopWatch()
	s.mu.Lock()
	for {
		if err := ctx.Err(); err != nil {
			s.mu.Unlock()
			return nil, err
		}
		block := s.blockUse[blockID]
		if block+bytes <= s.budget.MaxPerBlockBytes && s.globalUse+bytes <= s.budget.GlobalBytes {
			s.blockUse[blockID] = block + bytes
			s.globalUse += bytes
			s.mu.Unlock()
			var once sync.Once
			return func() {
				once.Do(func() {
					s.mu.Lock()
					s.globalUse -= bytes
					s.blockUse[blockID] -= bytes
					s.cond.Broadcast()
					s.mu.Unlock()
				})
			}, nil
		}
		if !recorded {
			reason := fmt.Sprintf(
				"queued: need=%dB global_used=%dB/%dB block_used=%dB/%dB",
				bytes, s.globalUse, s.budget.GlobalBytes, block, s.budget.MaxPerBlockBytes,
			)
			s.events = append(s.events, QueueEvent{BlockID: blockID, Label: label, Reason: reason})
			recorded = true
		}
		s.cond.Wait()
	}
}

func (s *Scheduler) Stats() SchedulerStats {
	s.mu.Lock()
	defer s.mu.Unlock()
	events := append([]QueueEvent(nil), s.events...)
	return SchedulerStats{
		MaxActiveObserved: s.maxActive,
		QueuedEvents:      events,
		CompletedJobs:     s.completed,
	}
}
