package core

import (
	"sync"
	"sync/atomic"
)

type PoolResult[T any] struct {
	Index int
	Value T
	Err   *Error
}

type poolJob[T any] struct {
	index  int
	run    func() (T, *Error)
	result chan<- PoolResult[T]
}

type BoundedPool[T any] struct {
	workers int
	buffer  int
	queues  []chan poolJob[T]
	rr      atomic.Uint64
	wg      sync.WaitGroup
}

func NewBoundedPool[T any](workers, buffer int) *BoundedPool[T] {
	if workers < 1 {
		workers = 1
	}
	if buffer < 1 {
		buffer = 1
	}
	p := &BoundedPool[T]{workers: workers, buffer: buffer, queues: make([]chan poolJob[T], workers)}
	for i := 0; i < workers; i++ {
		p.queues[i] = make(chan poolJob[T], buffer)
		p.wg.Add(1)
		go func(queue <-chan poolJob[T]) {
			defer p.wg.Done()
			for job := range queue {
				value, err := job.run()
				job.result <- PoolResult[T]{Index: job.index, Value: value, Err: err}
			}
		}(p.queues[i])
	}
	return p
}

func (p *BoundedPool[T]) Limits() (workers, buffer int) { return p.workers, p.buffer }

// Submit queues work. When every per-worker buffer is full it blocks; it never drops jobs.
func (p *BoundedPool[T]) Submit(index int, run func() (T, *Error)) <-chan PoolResult[T] {
	result := make(chan PoolResult[T], 1)
	job := poolJob[T]{index: index, run: run, result: result}
	start := int(p.rr.Add(1)-1) % p.workers
	for offset := 0; offset < p.workers; offset++ {
		idx := (start + offset) % p.workers
		select {
		case p.queues[idx] <- job:
			return result
		default:
		}
	}
	p.queues[start] <- job
	return result
}

func (p *BoundedPool[T]) Close() {
	for _, q := range p.queues {
		close(q)
	}
}

func (p *BoundedPool[T]) Wait() { p.wg.Wait() }

// RunIndexedParallel preserves input order in the returned slice while solving in parallel.
func RunIndexedParallel[T any](count int, resources ResourceConfig, run func(index int) (T, *Error)) ([]T, *Error) {
	pool := NewBoundedPool[T](resources.Workers, resources.BufferPerWorker)
	results := make([]<-chan PoolResult[T], count)
	for i := 0; i < count; i++ {
		i := i
		results[i] = pool.Submit(i, func() (T, *Error) { return run(i) })
	}
	pool.Close()
	values := make([]T, count)
	var firstErr *Error
	for i := 0; i < count; i++ {
		res := <-results[i]
		if res.Err != nil && firstErr == nil {
			firstErr = res.Err
		}
		values[i] = res.Value
	}
	pool.Wait()
	return values, firstErr
}
