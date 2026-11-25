package engine

import (
	"sync"

	"github.com/liang/collector-backpressure/bpderr"
)

type Budget struct {
	Workers        int `json:"workers"`
	PerAgentQueue  int `json:"per_agent_queue"`
	SchedulerQueue int `json:"scheduler_queue"`
}

func DefaultBudget() Budget {
	return Budget{Workers: 4, PerAgentQueue: 64, SchedulerQueue: 128}
}

func (b Budget) Validate() error {
	if b.Workers < 1 || b.Workers > 64 {
		return bpderr.Newf(bpderr.EBudgetOutOfRange, "worker_budget", "worker budget %d outside [1,64]", b.Workers).
			WithContext("min", 1).WithContext("max", 64)
	}
	if b.PerAgentQueue < 1 || b.PerAgentQueue > 100000 {
		return bpderr.Newf(bpderr.EBudgetOutOfRange, "pending_queue_budget", "per-agent queue budget %d outside [1,100000]", b.PerAgentQueue).
			WithContext("min", 1).WithContext("max", 100000)
	}
	if b.SchedulerQueue < 1 || b.SchedulerQueue > 100000 {
		return bpderr.Newf(bpderr.EBudgetOutOfRange, "scheduler_queue_budget", "scheduler queue budget %d outside [1,100000]", b.SchedulerQueue)
	}
	return nil
}

type Scheduler struct {
	budget Budget
	sem    chan struct{}
	adm    chan struct{}
}

func NewScheduler(b Budget) (*Scheduler, error) {
	if err := b.Validate(); err != nil {
		return nil, err
	}
	return &Scheduler{budget: b, sem: make(chan struct{}, b.Workers), adm: make(chan struct{}, b.SchedulerQueue)}, nil
}

// TrySubmit reserves one bounded admission slot without blocking. Tasks then
// queue on the worker semaphore inside Map. A full admission budget returns a
// stable business code instead of overflowing.
func (s *Scheduler) TrySubmit(taskID string) (func(), error) {
	select {
	case s.adm <- struct{}{}:
	default:
		return nil, bpderr.New(bpderr.EWorkerBudgetExhausted, "scheduler_queue", "worker admission budget is exhausted").
			WithContext("task_id", taskID).WithContext("scheduler_queue", s.budget.SchedulerQueue).
			WithContext("workers", s.budget.Workers)
	}
	var once sync.Once
	done := func() {
		once.Do(func() {
			<-s.adm
		})
	}
	return done, nil
}

// Map runs fn over items and preserves indexed errors/results. Concurrency
// never changes item order or domain state because callers give each worker a
// distinct owned state value.
func Map[T any, R any](s *Scheduler, items []T, fn func(index int, item T, workerIndex int) (R, error)) ([]R, error) {
	results := make([]R, len(items))
	if len(items) == 0 {
		return results, nil
	}
	if s == nil {
		for i, item := range items {
			r, err := fn(i, item, 0)
			if err != nil {
				return nil, err
			}
			results[i] = r
		}
		return results, nil
	}

	type indexedError struct {
		index int
		err   error
	}
	var wg sync.WaitGroup
	errCh := make(chan indexedError, len(items))
	for i := range items {
		done, err := s.TrySubmit("map-task")
		if err != nil {
			return nil, err
		}
		wg.Add(1)
		go func(index int) {
			defer wg.Done()
			defer done()
			s.sem <- struct{}{}
			defer func() { <-s.sem }()
			workerIndex := index % s.budget.Workers
			r, err := fn(index, items[index], workerIndex)
			if err != nil {
				errCh <- indexedError{index: index, err: err}
				return
			}
			results[index] = r
		}(i)
	}
	wg.Wait()
	close(errCh)
	for e := range errCh {
		if e.err != nil {
			return nil, e.err
		}
	}
	return results, nil
}
