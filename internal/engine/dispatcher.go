package engine

import (
	"sync"

	"quota-cluster/internal/bizerror"
)

// BoundedExecutor is the observable backpressure boundary for parallel gate
// work. When every worker is busy and the bounded submission channel is full,
// TrySubmit returns E_BACKPRESSURE_WORKER_BUDGET instead of growing without
// bound or dropping work.
type BoundedExecutor struct {
	tasks   chan func()
	workers int
	wg      sync.WaitGroup
}

func NewBoundedExecutor(workers, queueCapacity int) *BoundedExecutor {
	if workers <= 0 {
		workers = 1
	}
	if queueCapacity < 0 {
		queueCapacity = 0
	}
	e := &BoundedExecutor{tasks: make(chan func(), queueCapacity), workers: workers}
	for i := 0; i < workers; i++ {
		e.wg.Add(1)
		go func() {
			defer e.wg.Done()
			for task := range e.tasks {
				task()
			}
		}()
	}
	return e
}

// TrySubmit enqueues task only if bounded capacity is available.
func (e *BoundedExecutor) TrySubmit(task func()) error {
	select {
	case e.tasks <- task:
		return nil
	default:
		return bizerror.New(bizerror.CodeBackpressureWorkers, "/runtime/worker_budget", "all %d workers are busy and submission buffer is full", e.workers)
	}
}

// CloseAndWait stops intake and waits for queued work to finish.
func (e *BoundedExecutor) CloseAndWait() {
	close(e.tasks)
	e.wg.Wait()
}

// QueueAdmission enforces the per-tenant wait queue bound.
func QueueAdmission(currentLen, limit int) error {
	if limit > 0 && currentLen >= limit {
		return bizerror.New(bizerror.CodeQueueLimit, "/runtime/tenant_queue_limit", "tenant wait queue length %d reaches limit %d", currentLen, limit)
	}
	return nil
}
