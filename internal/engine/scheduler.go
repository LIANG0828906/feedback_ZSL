package engine

import (
	"sync"

	"featurekernel/internal/kernel"
)

type WorkerPool struct {
	workers int
	sem     chan struct{}
}

func NewWorkerPool(workers int) (*WorkerPool, error) {
	if workers <= 0 || workers > kernel.MaxWorkers {
		return nil, kernel.NewError(kernel.CodeBudgetWorkersOutOfRange, "worker budget is out of declared range", map[string]any{
			"workers": workers, "min": 1, "max": kernel.MaxWorkers,
		})
	}
	return &WorkerPool{workers: workers, sem: make(chan struct{}, workers)}, nil
}

func (p *WorkerPool) WorkerBudget() int { return p.workers }

func (p *WorkerPool) TrySubmit(task func()) error {
	select {
	case p.sem <- struct{}{}:
		go func() {
			defer func() { <-p.sem }()
			task()
		}()
		return nil
	default:
		return kernel.NewError(kernel.CodeBackpressureThread, "all declared workers are occupied", map[string]any{
			"worker_budget": p.workers,
		})
	}
}

func RunParallel(tasks []func(workerID int) error, workers int) []error {
	if len(tasks) == 0 {
		return nil
	}
	if workers > len(tasks) && len(tasks) > 0 {
		workers = len(tasks)
	}
	if workers <= 0 {
		workers = 1
	}
	pool, err := NewWorkerPool(workers)
	if err != nil {
		return []error{err}
	}

	jobs := make(chan int)
	var wg sync.WaitGroup
	errs := make([]error, len(tasks))
	var errMu sync.Mutex

	for workerID := 0; workerID < pool.WorkerBudget(); workerID++ {
		wg.Add(1)
		go func(workerID int) {
			defer wg.Done()
			for index := range jobs {
				if err := tasks[index](workerID); err != nil {
					errMu.Lock()
					errs[index] = err
					errMu.Unlock()
				}
			}
		}(workerID)
	}
	for index := range tasks {
		jobs <- index
	}
	close(jobs)
	wg.Wait()
	return errs
}
