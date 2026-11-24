package pipeline

import (
	"context"
	"sync"

	"iconbench/internal/model"
)

type ResourcePool struct {
	maxWorkers      int
	perThreadPixels int64
	capacityPixels  int64

	mu            sync.Mutex
	cond          *sync.Cond
	activeWorkers int
	usedPixels    int64
	queue         []model.QueueRecord
}

func NewResourcePool(budget model.Budget) *ResourcePool {
	workers := budget.WorkerThreads
	if workers <= 0 {
		workers = 1
	}
	per := int64(budget.PixelBufferPerThread)
	if per <= 0 {
		per = 65536
	}
	p := &ResourcePool{
		maxWorkers:      workers,
		perThreadPixels: per,
		capacityPixels:  int64(workers) * per,
	}
	p.cond = sync.NewCond(&p.mu)
	return p
}

func (p *ResourcePool) Run(ctx context.Context, record model.QueueRecord, pixels int, fn func() error) error {
	if int64(pixels) > p.perThreadPixels {
		return &model.Error{Code: model.CodeResource, Field: "pixel_buffer_per_thread", Expected: p.perThreadPixels, Actual: pixels,
			Message: "task pixel buffer exceeds per-thread limit"}
	}

	p.mu.Lock()
	queued := false
	for {
		if err := ctx.Err(); err != nil {
			p.mu.Unlock()
			return err
		}
		workerAvailable := p.activeWorkers < p.maxWorkers
		bufferAvailable := p.usedPixels+int64(pixels) <= p.capacityPixels
		if workerAvailable && bufferAvailable {
			break
		}
		if !queued {
			rec := record
			rec.Pixels = pixels
			switch {
			case !bufferAvailable:
				rec.Reason = "pixel-buffer-exhausted"
			default:
				rec.Reason = "worker-thread-budget"
			}
			p.queue = append(p.queue, rec)
			queued = true
		}
		p.cond.Wait()
	}
	p.activeWorkers++
	p.usedPixels += int64(pixels)
	p.mu.Unlock()

	defer func() {
		p.mu.Lock()
		p.activeWorkers--
		p.usedPixels -= int64(pixels)
		p.cond.Broadcast()
		p.mu.Unlock()
	}()
	return fn()
}

func (p *ResourcePool) QueueRecords() []model.QueueRecord {
	p.mu.Lock()
	defer p.mu.Unlock()
	out := make([]model.QueueRecord, len(p.queue))
	copy(out, p.queue)
	return out
}

func (p *ResourcePool) MaxWorkers() int { return p.maxWorkers }
