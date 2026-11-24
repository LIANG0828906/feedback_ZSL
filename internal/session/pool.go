package session

import (
	"context"
	"fmt"
	"sync"
	"time"

	"spatialworkbench/internal/werr"
)

type BufferEvent struct {
	At       time.Time `json:"at"`
	Reason   string    `json:"reason"`
	Samples  int       `json:"samples"`
	Used     int       `json:"used"`
	Capacity int       `json:"capacity"`
}

type sampleBudget struct {
	mu       sync.Mutex
	capacity int
	used     int
	events   []BufferEvent
}

func newSampleBudget(capacity int) *sampleBudget {
	return &sampleBudget{capacity: capacity}
}

func (b *sampleBudget) Acquire(ctx context.Context, samples int) *werr.Error {
	if samples < 0 {
		return werr.New(werr.CodeFrame, "negative sample buffer request %d", samples)
	}
	if samples > b.capacity {
		return werr.New(werr.CodeFrame, "frame requires %d buffered samples but global capacity is %d", samples, b.capacity)
	}
	recorded := false
	for {
		b.mu.Lock()
		if b.used+samples <= b.capacity {
			b.used += samples
			b.mu.Unlock()
			return nil
		}
		if !recorded {
			used := b.used
			b.events = append(b.events, BufferEvent{
				At:       time.Now(),
				Reason:   "sample-buffer-exhausted: task queued until an active worker releases its buffer",
				Samples:  samples,
				Used:     used,
				Capacity: b.capacity,
			})
			recorded = true
		}
		b.mu.Unlock()
		select {
		case <-ctx.Done():
			e := werr.New(werr.CodeFrame, "sample buffer wait canceled: %v", ctx.Err())
			e.Field = "bufferSamplesPerWorker"
			return e
		case <-time.After(2 * time.Millisecond):
		}
	}
}

func (b *sampleBudget) Release(samples int) {
	b.mu.Lock()
	b.used -= samples
	if b.used < 0 {
		b.used = 0
	}
	b.mu.Unlock()
}

func (b *sampleBudget) Events() []BufferEvent {
	b.mu.Lock()
	defer b.mu.Unlock()
	return append([]BufferEvent(nil), b.events...)
}

type taskResult struct {
	index int
	value any
	err   *werr.Error
}

type workerPool struct {
	workers   int
	perWorker int
	budget    *sampleBudget
	sem       chan struct{}
}

func newWorkerPool(workers, perWorker int) *workerPool {
	if workers < 1 {
		workers = 1
	}
	return &workerPool{
		workers:   workers,
		perWorker: perWorker,
		budget:    newSampleBudget(workers * perWorker),
		sem:       make(chan struct{}, workers),
	}
}

// Run executes independent tasks with bounded workers and a weighted sample
// budget. It preserves result indexes even when tasks finish out of order.
func (p *workerPool) Run(ctx context.Context, tasks []func(context.Context) (any, *werr.Error), samples []int) ([]taskResult, *werr.Error) {
	if len(tasks) != len(samples) {
		return nil, werr.New(werr.CodeState, "worker task and buffer metadata length mismatch")
	}
	for _, sampleCount := range samples {
		if sampleCount < 0 {
			return nil, werr.New(werr.CodeFrame, "negative sample buffer request %d", sampleCount)
		}
		if sampleCount > p.perWorker {
			e := werr.New(werr.CodeFrame, "task needs %d samples, per-worker limit is %d", sampleCount, p.perWorker)
			e.Field = "bufferSamplesPerWorker"
			return nil, e
		}
	}
	results := make([]taskResult, len(tasks))
	var wg sync.WaitGroup
	for i := range tasks {
		wg.Add(1)
		go func(index int) {
			defer wg.Done()
			select {
			case p.sem <- struct{}{}:
				defer func() { <-p.sem }()
			case <-ctx.Done():
				results[index] = taskResult{index: index, err: werr.New(werr.CodeState, "worker wait canceled: %v", ctx.Err())}
				return
			}
			if err := p.budget.Acquire(ctx, samples[index]); err != nil {
				results[index] = taskResult{index: index, err: err}
				return
			}
			defer p.budget.Release(samples[index])
			value, err := tasks[index](ctx)
			results[index] = taskResult{index: index, value: value, err: err}
		}(i)
	}
	wg.Wait()
	for _, result := range results {
		if result.err != nil {
			return results, result.err
		}
	}
	return results, nil
}

func (p *workerPool) Events() []BufferEvent {
	return p.budget.Events()
}

func formatBufferUsage(events []BufferEvent) string {
	if len(events) == 0 {
		return ""
	}
	last := events[len(events)-1]
	return fmt.Sprintf("%s (request=%d used=%d capacity=%d)", last.Reason, last.Samples, last.Used, last.Capacity)
}
