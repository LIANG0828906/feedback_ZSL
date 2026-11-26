package kernel

import (
	"sync"
)

// WorkerPool bounds concurrent settlement/gate work. Submit applies normal
// backpressure by waiting; TrySubmit is the deterministic overload probe used
// by diagnostics and fail-fast gate paths.
type WorkerPool struct {
	workers int
	sem     chan struct{}
	wg      sync.WaitGroup
}

func NewWorkerPool(workers int) *WorkerPool {
	if workers < 1 {
		workers = 1
	}
	return &WorkerPool{workers: workers, sem: make(chan struct{}, workers)}
}

func (p *WorkerPool) Workers() int { return p.workers }

func (p *WorkerPool) Active() int { return len(p.sem) }

func (p *WorkerPool) Submit(fn func()) {
	p.sem <- struct{}{}
	p.wg.Add(1)
	go func() {
		defer func() {
			<-p.sem
			p.wg.Done()
		}()
		fn()
	}()
}

func (p *WorkerPool) TrySubmit(fn func()) error {
	select {
	case p.sem <- struct{}{}:
	default:
		return NewBizError(CodeWorkerBusy, "worker budget %d is exhausted", p.workers).
			WithConstraint(ConstraintWorkerBudget).
			WithDetail("workers", p.workers).
			WithDetail("active", len(p.sem))
	}
	p.wg.Add(1)
	go func() {
		defer func() {
			<-p.sem
			p.wg.Done()
		}()
		fn()
	}()
	return nil
}

func (p *WorkerPool) Wait() { p.wg.Wait() }

// RunBounded runs one function per item while preserving the caller's result
// slice order. Concurrency is bounded, but admission order remains deterministic
// because callers sort before invoking this helper.
func RunBounded[T any](workers int, items []T, fn func(int, T) error) []error {
	if workers < 1 {
		workers = 1
	}
	errs := make([]error, len(items))
	sem := make(chan struct{}, workers)
	var wg sync.WaitGroup
	for i := range items {
		i := i
		item := items[i]
		sem <- struct{}{}
		wg.Add(1)
		go func() {
			defer func() { <-sem; wg.Done() }()
			errs[i] = fn(i, item)
		}()
	}
	wg.Wait()
	return errs
}
