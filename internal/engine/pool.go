package engine

import (
	"context"
	"sync"
	"sync/atomic"
)

type poolMetrics struct {
	tasks             atomic.Int64
	bufferBlockEvents atomic.Int64
	lastBlockReason   atomic.Value
}

type workerPool struct {
	workers   int
	buffer    int
	jobs      chan func()
	workerWG  sync.WaitGroup
	taskWG    sync.WaitGroup
	metrics   *poolMetrics
	closeOnce sync.Once
	submitMu  sync.Mutex
}

func newWorkerPool(workers, perWorkerBuffer int) *workerPool {
	if workers < 1 {
		workers = 1
	}
	if perWorkerBuffer < 1 {
		perWorkerBuffer = 1
	}
	p := &workerPool{
		workers: workers,
		buffer:  workers * perWorkerBuffer,
		jobs:    make(chan func(), workers*perWorkerBuffer),
		metrics: &poolMetrics{},
	}
	p.metrics.lastBlockReason.Store("")
	for i := 0; i < workers; i++ {
		p.workerWG.Add(1)
		go func() {
			defer p.workerWG.Done()
			for job := range p.jobs {
				job()
				p.taskWG.Done()
			}
		}()
	}
	return p
}

func (p *workerPool) Run(ctx context.Context, tasks int, fn func(int)) error {
	if tasks <= 0 {
		return nil
	}
	p.submitMu.Lock()
	defer p.submitMu.Unlock()
	var submitErr error
	for i := 0; i < tasks; i++ {
		if err := ctx.Err(); err != nil {
			submitErr = err
			break
		}
		job := func(index int) func() {
			return func() {
				p.metrics.tasks.Add(1)
				fn(index)
			}
		}(i)
		p.taskWG.Add(1)
		select {
		case p.jobs <- job:
		default:
			p.metrics.bufferBlockEvents.Add(1)
			p.metrics.lastBlockReason.Store("worker grid buffer exhausted; producer blocked in bounded queue")
			select {
			case p.jobs <- job:
			case <-ctx.Done():
				p.taskWG.Done()
				submitErr = ctx.Err()
				i = tasks
			}
		}
	}
	p.taskWG.Wait()
	return submitErr
}

func (p *workerPool) Metrics() RunMetrics {
	reason, _ := p.metrics.lastBlockReason.Load().(string)
	return RunMetrics{
		MaxParallelism:      p.workers,
		PerWorkerBufferSize: p.buffer / p.workers,
		Tasks:               p.metrics.tasks.Load(),
		BufferBlockEvents:   p.metrics.bufferBlockEvents.Load(),
		LastBlockReason:     reason,
	}
}

func (p *workerPool) Close() {
	p.closeOnce.Do(func() {
		close(p.jobs)
		p.workerWG.Wait()
	})
}
