package audiofp

import (
	"context"
	"runtime"
	"sync"
)

type ResourceLimits struct {
	MaxWorkers             int `json:"max_workers"`
	PerWorkerBufferSamples int `json:"per_worker_buffer_samples"`
	MaxQueuedJobs          int `json:"max_queued_jobs"`
}

func DefaultResourceLimits() ResourceLimits {
	workers := runtime.GOMAXPROCS(0)
	if workers < 1 {
		workers = 1
	}
	if workers > 4 {
		workers = 4
	}
	return ResourceLimits{
		MaxWorkers:             workers,
		PerWorkerBufferSamples: 32768,
		MaxQueuedJobs:          64,
	}
}

func (l ResourceLimits) Normalize() ResourceLimits {
	d := DefaultResourceLimits()
	if l.MaxWorkers <= 0 {
		l.MaxWorkers = d.MaxWorkers
	}
	if l.MaxWorkers > 64 {
		l.MaxWorkers = 64
	}
	if l.PerWorkerBufferSamples <= 0 {
		l.PerWorkerBufferSamples = d.PerWorkerBufferSamples
	}
	if l.MaxQueuedJobs < 0 {
		l.MaxQueuedJobs = 0
	}
	if l.MaxQueuedJobs == 0 {
		l.MaxQueuedJobs = l.MaxWorkers * 2
	}
	return l
}

func (l ResourceLimits) Validate(p Params) error {
	if l.MaxWorkers < 1 || l.MaxWorkers > 64 {
		return paraError("max_workers", "must be in [1,64], got %d", l.MaxWorkers)
	}
	if l.PerWorkerBufferSamples < p.FrameLength {
		return paraError("per_worker_buffer_samples",
			"buffer capacity %d is smaller than frame_length %d",
			l.PerWorkerBufferSamples, p.FrameLength)
	}
	if l.MaxQueuedJobs < 0 || l.MaxQueuedJobs > 1000000 {
		return paraError("max_queued_jobs", "invalid queue size %d", l.MaxQueuedJobs)
	}
	return nil
}

// BufferJob runs on a worker. buf has length NeededSamples and is reused by
// that worker after Handle returns.
type BufferJob struct {
	NeededSamples int
	Handle        func(buf []float64) error
}

type queuedJob struct {
	job BufferJob
}

// WorkerPool is a fixed worker pool with a bounded queue and one bounded
// sample buffer per worker. Submit blocks when the queue is full, providing
// backpressure instead of dropping work.
type WorkerPool struct {
	limits    ResourceLimits
	jobs      chan queuedJob
	wg        sync.WaitGroup
	closeOnce sync.Once
}

func NewWorkerPool(limits ResourceLimits) (*WorkerPool, error) {
	limits = limits.Normalize()
	if limits.MaxWorkers < 1 {
		return nil, paraError("max_workers", "must be positive")
	}
	pool := &WorkerPool{
		limits: limits,
		jobs:   make(chan queuedJob, limits.MaxQueuedJobs),
	}
	for w := 0; w < limits.MaxWorkers; w++ {
		pool.wg.Add(1)
		buffer := make([]float64, limits.PerWorkerBufferSamples)
		go func() {
			defer pool.wg.Done()
			for item := range pool.jobs {
				needed := item.job.NeededSamples
				if needed < 0 || needed > cap(buffer) {
					// Submit checks this before queueing. This branch protects
					// internal callers from slicing outside the buffer.
					_ = item.job.Handle(nil)
					continue
				}
				_ = item.job.Handle(buffer[:needed])
			}
		}()
	}
	return pool, nil
}

func (p *WorkerPool) Limits() ResourceLimits { return p.limits }

// Submit enqueues a job. It blocks while the bounded queue is full.
func (p *WorkerPool) Submit(ctx context.Context, job BufferJob) error {
	if job.NeededSamples < 1 || job.NeededSamples > p.limits.PerWorkerBufferSamples {
		return paraError("needed_samples", "job needs %d samples, buffer limit is %d",
			job.NeededSamples, p.limits.PerWorkerBufferSamples)
	}
	if job.Handle == nil {
		return newError(CodeSchema, "handle", "job handle is nil")
	}
	select {
	case p.jobs <- queuedJob{job: job}:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

// Close stops accepting work and waits for all queued jobs to finish.
func (p *WorkerPool) Close() error {
	p.closeOnce.Do(func() { close(p.jobs) })
	p.wg.Wait()
	return nil
}

// ExtractFrames extracts every complete frame. Results are written to unique
// indices and returned in canonical frame order, independent of worker
// completion order.
func ExtractFrames(samples []float64, sampleRate int, params Params, limits ResourceLimits) ([]Fingerprint, error) {
	params = params.Normalize()
	if err := params.Validate(); err != nil {
		return nil, err
	}
	limits = limits.Normalize()
	if err := limits.Validate(params); err != nil {
		return nil, err
	}

	count := FrameCount(len(samples), params)
	frames := make([]Fingerprint, count)
	if count == 0 {
		return frames, nil
	}

	workers := limits.MaxWorkers
	if workers > count {
		workers = count
	}
	jobChan := make(chan int, limits.MaxQueuedJobs)
	var wg sync.WaitGroup
	for w := 0; w < workers; w++ {
		wg.Add(1)
		buffer := make([]float64, params.FrameLength, limits.PerWorkerBufferSamples)
		go func() {
			defer wg.Done()
			for index := range jobChan {
				start := FrameStart(index, params)
				copy(buffer, samples[start:start+params.FrameLength])
				frames[index] = ExtractFrameBuffer(buffer, index, start, sampleRate, params)
			}
		}()
	}

	for index := 0; index < count; index++ {
		jobChan <- index
	}
	close(jobChan)
	wg.Wait()
	return frames, nil
}
