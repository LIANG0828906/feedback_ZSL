package engine

import (
	"context"
	"sync"
	"sync/atomic"

	"fixedsampler/internal/model"
)

type nodeResult struct {
	artifact *model.LevelArtifact
	err      error
}

type nodeJob struct {
	seq        int
	chunkIndex int
	work       func() (*model.LevelArtifact, error)
	done       chan nodeResult
}

type WorkerPool struct {
	workers     int
	bufferLimit int
	queues      []chan nodeJob
	wg          sync.WaitGroup
	next        atomic.Uint64
	eventMu     sync.Mutex
	events      []model.QueueEvent
}

func NewWorkerPool(workers, bufferLimit int) *WorkerPool {
	if workers < 1 {
		workers = 1
	}
	if bufferLimit < 1 {
		bufferLimit = 1
	}
	p := &WorkerPool{workers: workers, bufferLimit: bufferLimit, queues: make([]chan nodeJob, workers)}
	for i := range p.queues {
		p.queues[i] = make(chan nodeJob, bufferLimit)
		p.wg.Add(1)
		go func(id int, ch <-chan nodeJob) {
			defer p.wg.Done()
			for job := range ch {
				art, err := job.work()
				job.done <- nodeResult{artifact: art, err: err}
			}
		}(i, p.queues[i])
	}
	return p
}

func (p *WorkerPool) Submit(ctx context.Context, chunkIndex, seq, chunkX, chunkY int, work func() (*model.LevelArtifact, error)) (<-chan nodeResult, error) {
	worker := int(p.next.Add(1)-1) % p.workers
	ch := p.queues[worker]
	done := make(chan nodeResult, 1)
	job := nodeJob{seq: seq, chunkIndex: chunkIndex, work: work, done: done}

	if len(ch) >= cap(ch) {
		ev := model.QueueEvent{
			ChunkIndex: chunkIndex, StageSeq: seq, Worker: worker,
			Reason:      "per_worker_sampling_buffer_full_blocking_enqueue",
			QueueLength: len(ch), BufferLimit: p.bufferLimit,
		}
		p.eventMu.Lock()
		p.events = append(p.events, ev)
		p.eventMu.Unlock()
	}

	select {
	case ch <- job:
		return done, nil
	case <-ctx.Done():
		return nil, ctx.Err()
	}
}

func (p *WorkerPool) Events() []model.QueueEvent {
	p.eventMu.Lock()
	defer p.eventMu.Unlock()
	return append([]model.QueueEvent(nil), p.events...)
}

func (p *WorkerPool) Close() {
	for _, ch := range p.queues {
		close(ch)
	}
	p.wg.Wait()
}
