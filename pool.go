package syncgame

import (
	"context"
	"fmt"
	"sync"
	"sync/atomic"
	"time"
)

const MaxParallelism = 8

type PoolConfig struct {
	Workers         int
	QueuePerWorker  int
	StateBufferSize int
}

type PoolStats struct {
	Workers          int              `json:"workers"`
	QueuePerWorker   int              `json:"queue_per_worker"`
	StateBufferSize  int              `json:"state_buffer_size"`
	Completed        int64            `json:"completed"`
	Failed           int64            `json:"failed"`
	QueueBlocks      int64            `json:"queue_blocks"`
	QueueBlockNanos  int64            `json:"queue_block_nanos"`
	BufferBlocks     int64            `json:"buffer_blocks"`
	BufferBlockNanos int64            `json:"buffer_block_nanos"`
	BlockReasons     map[string]int64 `json:"block_reasons"`
}

type poolJob struct {
	name string
	run  func() (any, error)
	out  chan poolResult
}

type poolResult struct {
	value any
	err   error
}

type WorkerPool struct {
	config PoolConfig
	queues []chan poolJob
	rr     atomic.Uint64
	closed atomic.Bool
	wg     sync.WaitGroup

	mu       sync.Mutex
	stats    PoolStats
	closeErr error
}

func NewWorkerPool(config PoolConfig) *WorkerPool {
	if config.Workers < 1 {
		config.Workers = 1
	}
	if config.Workers > MaxParallelism {
		config.Workers = MaxParallelism
	}
	if config.QueuePerWorker < 1 {
		config.QueuePerWorker = 1
	}
	if config.StateBufferSize < 1 {
		config.StateBufferSize = config.QueuePerWorker
	}
	pool := &WorkerPool{
		config: config,
		queues: make([]chan poolJob, config.Workers),
		stats: PoolStats{
			Workers:         config.Workers,
			QueuePerWorker:  config.QueuePerWorker,
			StateBufferSize: config.StateBufferSize,
			BlockReasons:    make(map[string]int64),
		},
	}
	for i := range pool.queues {
		queue := make(chan poolJob, config.QueuePerWorker)
		pool.queues[i] = queue
		pool.wg.Add(1)
		go pool.worker(queue)
	}
	return pool
}

func (p *WorkerPool) worker(queue <-chan poolJob) {
	defer p.wg.Done()
	for job := range queue {
		value, err := safeRun(job.run)
		p.mu.Lock()
		if err != nil {
			p.stats.Failed++
		} else {
			p.stats.Completed++
		}
		p.mu.Unlock()
		job.out <- poolResult{value: value, err: err}
	}
}

func (p *WorkerPool) Submit(ctx context.Context, name string, run func() (any, error)) (any, error) {
	if p.closed.Load() {
		return nil, fmt.Errorf("worker pool is closed")
	}
	if run == nil {
		return nil, fmt.Errorf("worker task %q is nil", name)
	}
	job := poolJob{name: name, run: run, out: make(chan poolResult, 1)}
	queue := p.queues[int(p.rr.Add(1)-1)%len(p.queues)]
	start := time.Now()
	select {
	case queue <- job:
	default:
		p.recordBlock("queue_full:"+name, start, true)
		select {
		case queue <- job:
		case <-ctx.Done():
			return nil, ctx.Err()
		}
	}
	select {
	case result := <-job.out:
		return result.value, result.err
	case <-ctx.Done():
		return nil, ctx.Err()
	}
}

func (p *WorkerPool) SubmitBatch(ctx context.Context, names []string, runs []func() (any, error)) ([]any, error) {
	if len(names) != len(runs) {
		return nil, fmt.Errorf("worker batch names and runs differ in length")
	}
	type completed struct {
		index int
		value any
		err   error
	}
	results := make(chan completed, p.config.StateBufferSize)
	var wg sync.WaitGroup
	for i := range runs {
		name := names[i]
		run := runs[i]
		wg.Add(1)
		go func(index int, taskName string, task func() (any, error)) {
			defer wg.Done()
			value, err := p.Submit(ctx, taskName, task)
			result := completed{index: index, value: value, err: err}
			start := time.Now()
			select {
			case results <- result:
			default:
				p.recordBlock("result_buffer_full:"+taskName, start, false)
				results <- result
			}
		}(i, name, run)
	}
	go func() {
		wg.Wait()
		close(results)
	}()
	values := make([]any, len(runs))
	var firstErr error
	for result := range results {
		values[result.index] = result.value
		if result.err != nil && firstErr == nil {
			firstErr = result.err
		}
	}
	return values, firstErr
}

func (p *WorkerPool) Close() error {
	if p.closed.Swap(true) {
		return p.closeErr
	}
	for _, queue := range p.queues {
		close(queue)
	}
	p.wg.Wait()
	return nil
}

func (p *WorkerPool) Stats() PoolStats {
	p.mu.Lock()
	defer p.mu.Unlock()
	stats := p.stats
	stats.BlockReasons = make(map[string]int64, len(p.stats.BlockReasons))
	for key, value := range p.stats.BlockReasons {
		stats.BlockReasons[key] = value
	}
	return stats
}

func (p *WorkerPool) recordBlock(reason string, start time.Time, queue bool) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if queue {
		p.stats.QueueBlocks++
		p.stats.QueueBlockNanos += time.Since(start).Nanoseconds()
	} else {
		p.stats.BufferBlocks++
		p.stats.BufferBlockNanos += time.Since(start).Nanoseconds()
	}
	p.stats.BlockReasons[reason]++
}

func safeRun(run func() (any, error)) (value any, err error) {
	defer func() {
		if recovered := recover(); recovered != nil {
			err = fmt.Errorf("worker panic: %v", recovered)
		}
	}()
	return run()
}

// BoundedBuffer never drops values. When full it records the blocking reason
// and waits for either capacity or context cancellation.
type BoundedBuffer[T any] struct {
	ch         chan T
	mu         sync.Mutex
	blocks     int64
	blockNanos int64
	reasons    map[string]int64
}

func NewBoundedBuffer[T any](capacity int) *BoundedBuffer[T] {
	if capacity < 1 {
		capacity = 1
	}
	return &BoundedBuffer[T]{
		ch:      make(chan T, capacity),
		reasons: make(map[string]int64),
	}
}

func (b *BoundedBuffer[T]) Push(ctx context.Context, value T, reason string) error {
	start := time.Now()
	select {
	case b.ch <- value:
		return nil
	default:
	}
	b.mu.Lock()
	b.blocks++
	if reason == "" {
		reason = "state_buffer_full"
	}
	b.reasons[reason]++
	b.mu.Unlock()
	select {
	case b.ch <- value:
		b.mu.Lock()
		b.blockNanos += time.Since(start).Nanoseconds()
		b.mu.Unlock()
		return nil
	case <-ctx.Done():
		b.mu.Lock()
		b.blockNanos += time.Since(start).Nanoseconds()
		b.mu.Unlock()
		return ctx.Err()
	}
}

func (b *BoundedBuffer[T]) Pop(ctx context.Context) (T, error) {
	var zero T
	select {
	case value := <-b.ch:
		return value, nil
	case <-ctx.Done():
		return zero, ctx.Err()
	}
}

func (b *BoundedBuffer[T]) Len() int { return len(b.ch) }
func (b *BoundedBuffer[T]) Cap() int { return cap(b.ch) }

func (b *BoundedBuffer[T]) Stats() (blocks, blockNanos int64, reasons map[string]int64) {
	b.mu.Lock()
	defer b.mu.Unlock()
	reasons = make(map[string]int64, len(b.reasons))
	for key, value := range b.reasons {
		reasons[key] = value
	}
	return b.blocks, b.blockNanos, reasons
}
