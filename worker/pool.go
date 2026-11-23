package worker

import (
	"context"
	"fmt"
	"runtime/debug"
	"sync"
)

type Job struct {
	ID int
	Fn func(context.Context) (any, error)
}

type Result struct {
	ID      int
	Value   any
	Err     error
	pending chan Result
}

type task struct {
	job Job
	ctx context.Context
	out chan Result
}

// Pool is a fixed-size worker pool. Each worker has an independent bounded
// queue; producers block when that worker's budget is exhausted. Results are
// returned by submission index, never by completion order.
type Pool struct {
	workers int
	buffer  int
	queues  []chan task
	closeCh chan struct{}
	wg      sync.WaitGroup
	runMu   sync.Mutex
	closed  bool
}

func New(workers, bufferPerWorker int) (*Pool, error) {
	if workers <= 0 {
		return nil, fmt.Errorf("workers must be positive")
	}
	if bufferPerWorker <= 0 {
		return nil, fmt.Errorf("buffer per worker must be positive")
	}
	p := &Pool{
		workers: workers,
		buffer:  bufferPerWorker,
		queues:  make([]chan task, workers),
		closeCh: make(chan struct{}),
	}
	for i := 0; i < workers; i++ {
		p.queues[i] = make(chan task, bufferPerWorker)
		p.wg.Add(1)
		go p.worker(i, p.queues[i])
	}
	return p, nil
}

func (p *Pool) Workers() int         { return p.workers }
func (p *Pool) BufferPerWorker() int { return p.buffer }

func (p *Pool) worker(id int, queue <-chan task) {
	defer p.wg.Done()
	for {
		select {
		case <-p.closeCh:
			return
		case t, ok := <-queue:
			if !ok {
				return
			}
			result := Result{ID: t.job.ID}
			func() {
				defer func() {
					if recovered := recover(); recovered != nil {
						result.Err = fmt.Errorf("worker %d panic: %v\n%s", id, recovered, debug.Stack())
					}
				}()
				result.Value, result.Err = t.job.Fn(t.ctx)
			}()
			t.out <- result
		}
	}
}

// Run executes all jobs in parallel while preserving result ordering by Job.ID.
// Submission blocks when queues are full and therefore never drops work.
func (p *Pool) Run(ctx context.Context, jobs []Job) ([]Result, error) {
	p.runMu.Lock()
	defer p.runMu.Unlock()
	if p.closed {
		return nil, fmt.Errorf("worker pool is closed")
	}
	results := make([]Result, len(jobs))
	if len(jobs) == 0 {
		return results, nil
	}
	for i, job := range jobs {
		if job.ID < 0 || job.ID >= len(jobs) {
			return nil, fmt.Errorf("job id %d is outside [0,%d)", job.ID, len(jobs))
		}
		if job.Fn == nil {
			return nil, fmt.Errorf("job %d has no function", job.ID)
		}
		t := task{job: job, ctx: ctx, out: make(chan Result, 1)}
		select {
		case p.queues[i%p.workers] <- t:
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-p.closeCh:
			return nil, fmt.Errorf("worker pool closed while submitting")
		}
		results[i] = Result{ID: job.ID}
		results[i].pending = t.out
	}
	for i := range results {
		select {
		case result := <-results[i].pending:
			results[i] = result
		case <-ctx.Done():
			return nil, ctx.Err()
		}
	}
	return results, nil
}

func (p *Pool) Close() {
	p.runMu.Lock()
	defer p.runMu.Unlock()
	if p.closed {
		return
	}
	p.closed = true
	close(p.closeCh)
	p.wg.Wait()
}
