package solver

import (
	"context"
	"sync"
	"time"

	"levelacoustic/internal/model"
)

type workerJob struct {
	id       string
	samples  int
	enqueued time.Time
	run      func([]int64) error
}

type workerPool struct {
	workers    int
	maxBuffers int
	jobs       chan workerJob
	buffers    chan []int64
	wg         sync.WaitGroup

	mu               sync.Mutex
	closed           bool
	jobsSeen         int
	events           []model.ResourceEvent
	totalWaitMicros  int64
	maxWaitMillis    int64
	perBufferSamples int
	firstErr         error
}

func newWorkerPool(workers, maxBuffers, perBufferSamples int) *workerPool {
	if workers < 1 {
		workers = 1
	}
	if maxBuffers < 1 {
		maxBuffers = workers
	}
	if perBufferSamples < 1 {
		perBufferSamples = model.DefaultCurveSamples
	}
	p := &workerPool{
		workers:          workers,
		maxBuffers:       maxBuffers,
		jobs:             make(chan workerJob, workers*4),
		buffers:          make(chan []int64, maxBuffers),
		perBufferSamples: perBufferSamples,
	}
	for i := 0; i < maxBuffers; i++ {
		p.buffers <- make([]int64, perBufferSamples)
	}
	p.wg.Add(workers)
	for i := 0; i < workers; i++ {
		go p.worker()
	}
	return p
}

func (p *workerPool) worker() {
	defer p.wg.Done()
	for job := range p.jobs {
		var buf []int64
		select {
		case buf = <-p.buffers:
			waited := time.Since(job.enqueued)
			p.mu.Lock()
			if waited.Microseconds() > 0 {
				p.events = append(p.events, model.ResourceEvent{
					JobID:         job.id,
					Reason:        "sample_buffer_exhausted",
					WaitMicros:    waited.Microseconds(),
					WaitMillis:    waited.Milliseconds(),
					BufferSamples: p.perBufferSamples,
				})
			}
			p.totalWaitMicros += waited.Microseconds()
			if waited.Milliseconds() > p.maxWaitMillis {
				p.maxWaitMillis = waited.Milliseconds()
			}
			p.mu.Unlock()
			if err := job.run(buf); err != nil {
				p.mu.Lock()
				if p.firstErr == nil {
					p.firstErr = err
				}
				p.mu.Unlock()
			}
			p.buffers <- buf
		}
	}
}

func (p *workerPool) submit(ctx context.Context, job workerJob) error {
	p.mu.Lock()
	if p.closed {
		p.mu.Unlock()
		return model.NewError(model.ErrResource, "worker pool is closed")
	}
	p.jobsSeen++
	job.enqueued = time.Now()
	p.mu.Unlock()
	select {
	case p.jobs <- job:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

func (p *workerPool) close() error {
	p.mu.Lock()
	if p.closed {
		err := p.firstErr
		p.mu.Unlock()
		return err
	}
	p.closed = true
	close(p.jobs)
	p.mu.Unlock()
	p.wg.Wait()
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.firstErr
}

func (p *workerPool) summary() model.ResourceSummary {
	p.mu.Lock()
	defer p.mu.Unlock()
	events := append([]model.ResourceEvent(nil), p.events...)
	queued := 0
	for _, event := range events {
		if event.WaitMicros > 0 {
			queued++
		}
	}
	return model.ResourceSummary{
		MaxParallel:      p.workers,
		BufferCount:      p.maxBuffers,
		MaxBufferSamples: p.perBufferSamples,
		WorkerCount:      p.workers,
		Jobs:             p.jobsSeen,
		QueuedJobs:       queued,
		TotalWaitMicros:  p.totalWaitMicros,
		TotalWaitMillis:  p.totalWaitMicros / 1000,
		MaxWaitMillis:    p.maxWaitMillis,
		Events:           events,
	}
}
