package parallel

import (
	"context"
	"sync"

	"maintenanceorchestrator/internal/domain"
)

type Job func(context.Context) error

type Scheduler struct {
	threads    int
	queueLimit int
	sem        chan struct{}

	mu     sync.Mutex
	queues map[string]chan Job
	wg     sync.WaitGroup
	ctx    context.Context
	cancel context.CancelFunc
	closed bool
}

func NewScheduler(threads, queueLimit int) (*Scheduler, error) {
	if err := domain.ValidateConcurrency(threads, queueLimit); err != nil {
		return nil, err
	}
	ctx, cancel := context.WithCancel(context.Background())
	s := &Scheduler{
		threads: threads, queueLimit: queueLimit,
		sem:    make(chan struct{}, threads),
		queues: map[string]chan Job{},
		ctx:    ctx, cancel: cancel,
	}
	return s, nil
}

func (s *Scheduler) Submit(nodeID string, fn Job) error {
	s.mu.Lock()
	if s.closed {
		s.mu.Unlock()
		return domain.NewError(domain.EBackpressureQueue, "$.scheduler.closed", "scheduler is closed")
	}
	q, ok := s.queues[nodeID]
	if !ok {
		q = make(chan Job, s.queueLimit)
		s.queues[nodeID] = q
		s.wg.Add(1)
		go s.runQueue(q)
	}
	s.mu.Unlock()

	select {
	case q <- fn:
		return nil
	default:
		return domain.NewError(domain.EBackpressureQueue, "$.policies.per_node_queue_limit",
			"node %q queue limit %d exhausted", nodeID, s.queueLimit).
			WithDetailsSafe(map[string]any{"node_id": nodeID, "queue_limit": s.queueLimit})
	}
}

func (s *Scheduler) runQueue(q chan Job) {
	defer s.wg.Done()
	for fn := range q {
		s.sem <- struct{}{}
		func(job Job) {
			defer func() { <-s.sem }()
			_ = job(s.ctx)
		}(fn)
	}
}

func (s *Scheduler) Wait() error {
	// Job-level waiting is handled by callers; Close performs worker shutdown.
	return nil
}

func (s *Scheduler) Close() {
	s.mu.Lock()
	if s.closed {
		s.mu.Unlock()
		return
	}
	s.closed = true
	for _, q := range s.queues {
		close(q)
	}
	s.mu.Unlock()
	s.wg.Wait()
	s.cancel()
}

func (s *Scheduler) TryExecute(fn Job) error {
	select {
	case s.sem <- struct{}{}:
		defer func() { <-s.sem }()
		return fn(s.ctx)
	default:
		return domain.NewError(domain.EBackpressureThread, "$.policies.thread_budget",
			"thread budget %d exhausted", s.threads).
			WithDetailsSafe(map[string]any{"thread_budget": s.threads})
	}
}

func MapIndexed[T any, R any](items []T, threads int, queueLimit int, key func(T) string, fn func(T) (R, error)) ([]R, []error) {
	s, err := NewScheduler(threads, queueLimit)
	if err != nil {
		return nil, []error{err}
	}

	var mu sync.Mutex
	results := make([]R, len(items))
	var errs []error
	var wg sync.WaitGroup
	for idx, item := range items {
		item := item
		i := idx
		nodeID := key(item)
		wg.Add(1)
		if submitErr := s.Submit(nodeID, func(context.Context) error {
			defer wg.Done()
			val, callErr := fn(item)
			mu.Lock()
			results[i] = val
			if callErr != nil {
				errs = append(errs, callErr)
			}
			mu.Unlock()
			return callErr
		}); submitErr != nil {
			wg.Done()
			errs = append(errs, submitErr)
		}
	}
	wg.Wait()
	s.Close()
	return results, errs
}
