package acoustics

import (
	"context"
	"fmt"
	"sync"
	"sync/atomic"
)

// QueueEvent explains backpressure instead of silently dropping work.
type QueueEvent struct {
	Seq     int64  `json:"seq"`
	TaskID  string `json:"task_id"`
	Worker  int    `json:"worker,omitempty"`
	Reason  string `json:"reason"`
	Message string `json:"message"`
}

type Task struct {
	ID string
	Fn func() error
}

type poolJob struct {
	task Task
	done chan error
}

// Pool is a fixed worker pool with a bounded channel per worker.
type Pool struct {
	workers  int
	buffer   int
	queues   []chan poolJob
	wg       sync.WaitGroup
	rr       uint64
	eventMu  sync.Mutex
	events   []QueueEvent
	eventSeq int64
}

func NewPool(maxWorkers, bufferPerWorker int) *Pool {
	if maxWorkers < 1 {
		maxWorkers = 1
	}
	if bufferPerWorker < 1 {
		bufferPerWorker = 1
	}
	p := &Pool{workers: maxWorkers, buffer: bufferPerWorker}
	for i := 0; i < maxWorkers; i++ {
		q := make(chan poolJob, bufferPerWorker)
		p.queues = append(p.queues, q)
		p.wg.Add(1)
		go p.worker(i, q)
	}
	return p
}

func (p *Pool) worker(id int, q <-chan poolJob) {
	defer p.wg.Done()
	for job := range q {
		err := p.safeRun(job.task)
		job.done <- err
	}
}

func (p *Pool) safeRun(task Task) (err error) {
	defer func() {
		if r := recover(); r != nil {
			err = &DomainError{Code: CodeState, ObjectID: task.ID, Field: "worker_panic", Message: fmt.Sprintf("isolated task panic: %v", r)}
		}
	}()
	return task.Fn()
}

// Run executes tasks and returns one error per task index. Buffer exhaustion
// blocks submission and records a diagnostic event.
func (p *Pool) Run(ctx context.Context, tasks []Task) []error {
	jobs := make([]poolJob, len(tasks))
	for i, task := range tasks {
		job := poolJob{task: task, done: make(chan error, 1)}
		jobs[i] = job
		if err := p.submit(ctx, job); err != nil {
			return []error{err}
		}
	}
	errs := make([]error, len(tasks))
	for i, job := range jobs {
		select {
		case errs[i] = <-job.done:
		case <-ctx.Done():
			errs[i] = ctx.Err()
		}
	}
	return errs
}

func (p *Pool) submit(ctx context.Context, job poolJob) error {
	// Fast path: any worker with buffer space.
	for i, q := range p.queues {
		select {
		case q <- job:
			_ = i
			return nil
		default:
		}
	}

	idx := int(atomic.AddUint64(&p.rr, 1)-1) % p.workers
	p.recordEvent(job.task.ID, idx, "buffer_full", "per-worker state buffer is exhausted; task is blocked until a slot is available")
	select {
	case p.queues[idx] <- job:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

func (p *Pool) recordEvent(taskID string, worker int, reason, message string) {
	p.eventMu.Lock()
	defer p.eventMu.Unlock()
	p.eventSeq++
	p.events = append(p.events, QueueEvent{Seq: p.eventSeq, TaskID: taskID, Worker: worker, Reason: reason, Message: message})
}

func (p *Pool) QueueEvents() []QueueEvent {
	p.eventMu.Lock()
	defer p.eventMu.Unlock()
	return append([]QueueEvent{}, p.events...)
}

func (p *Pool) Close() {
	for _, q := range p.queues {
		close(q)
	}
	p.wg.Wait()
}
