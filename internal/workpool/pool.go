package workpool

import (
	"context"
	"fmt"
	"sync"

	"iconseg/internal/core"
)

type Task struct {
	Pixels int
	Run    func(scratch []byte) (any, error)
}

type Result struct {
	Value any
	Err   error
}

type Pool struct {
	workers         int
	perThreadPixels int
	budget          *weightedSemaphore
	runMu           sync.Mutex
}

func New(workers, perThreadPixelBuffer int) (*Pool, error) {
	if workers < 1 || workers > 32 {
		return nil, core.ParaError("worker_budget", "[1,32]", "worker count is outside the legal interval")
	}
	if perThreadPixelBuffer < 1 {
		return nil, core.ParaError("per_thread_pixel_buffer", "[1,inf)", "per-thread pixel buffer must be positive")
	}
	return &Pool{
		workers:         workers,
		perThreadPixels: perThreadPixelBuffer,
		budget:          newWeightedSemaphore(workers * perThreadPixelBuffer),
	}, nil
}

// Run executes every task or returns a context error. No task is dropped:
// admission blocks while the aggregate pixel budget is exhausted. Results are
// returned in submission order, independent of worker completion order.
func (p *Pool) Run(ctx context.Context, tasks []Task) ([]Result, error) {
	p.runMu.Lock()
	defer p.runMu.Unlock()
	results := make([]Result, len(tasks))
	if len(tasks) == 0 {
		return results, nil
	}
	for i, task := range tasks {
		if task.Pixels < 0 || task.Pixels > p.perThreadPixels {
			return nil, core.ParaError("per_thread_pixel_buffer",
				fmt.Sprintf("[0,%d]", p.perThreadPixels),
				fmt.Sprintf("task %d requires %d pixels, exceeding the per-thread buffer", i, task.Pixels))
		}
		if task.Run == nil {
			return nil, core.ParaError("task", "non-nil closure", fmt.Sprintf("task %d has no body", i))
		}
	}
	type job struct {
		index int
		task  Task
	}
	jobs := make(chan job)
	var wg sync.WaitGroup
	var scratchWG sync.WaitGroup
	for workerID := 0; workerID < p.workers; workerID++ {
		wg.Add(1)
		scratchWG.Add(1)
		go func() {
			defer wg.Done()
			scratch := make([]byte, p.perThreadPixels*4)
			scratchWG.Done()
			for item := range jobs {
				value, err := item.task.Run(scratch)
				results[item.index] = Result{Value: value, Err: err}
				p.budget.release(item.task.Pixels)
			}
		}()
	}
	scratchWG.Wait()
	cancelErr := error(nil)
	for index, task := range tasks {
		if err := p.budget.acquire(ctx, task.Pixels); err != nil {
			cancelErr = err
			break
		}
		select {
		case jobs <- job{index: index, task: task}:
		case <-ctx.Done():
			p.budget.release(task.Pixels)
			cancelErr = ctx.Err()
		}
		if cancelErr != nil {
			break
		}
	}
	close(jobs)
	wg.Wait()
	if cancelErr != nil {
		return results, cancelErr
	}
	return results, nil
}

type weightedSemaphore struct {
	mu    sync.Mutex
	cond  *sync.Cond
	total int
	used  int
}

func newWeightedSemaphore(total int) *weightedSemaphore {
	s := &weightedSemaphore{total: total}
	s.cond = sync.NewCond(&s.mu)
	return s
}

func (s *weightedSemaphore) acquire(ctx context.Context, n int) error {
	if n == 0 {
		return nil
	}
	if n > s.total {
		return fmt.Errorf("requested pixel budget %d exceeds total %d", n, s.total)
	}
	stop := make(chan struct{})
	go func() {
		select {
		case <-ctx.Done():
			s.mu.Lock()
			s.cond.Broadcast()
			s.mu.Unlock()
		case <-stop:
		}
	}()
	defer close(stop)
	s.mu.Lock()
	defer s.mu.Unlock()
	for s.used+n > s.total {
		if err := ctx.Err(); err != nil {
			return err
		}
		s.cond.Wait()
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	s.used += n
	return nil
}

func (s *weightedSemaphore) release(n int) {
	if n == 0 {
		return
	}
	s.mu.Lock()
	s.used -= n
	if s.used < 0 {
		panic("weighted semaphore released more than acquired")
	}
	s.cond.Broadcast()
	s.mu.Unlock()
}
