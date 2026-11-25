package pipeline

import (
	"context"
	"sync"
)

// BoundedPool limits both worker concurrency and the number of mesh buffers
// that may be alive/queued. Submit blocks when the buffer budget is exhausted;
// tasks are never dropped.
type BoundedPool struct {
	workers chan struct{}
	buffers chan struct{}
	wg      sync.WaitGroup
}

func NewBoundedPool(maxParallel, buffersPerWorker int) *BoundedPool {
	if maxParallel <= 0 {
		maxParallel = 1
	}
	if buffersPerWorker <= 0 {
		buffersPerWorker = 1
	}
	return &BoundedPool{
		workers: make(chan struct{}, maxParallel),
		buffers: make(chan struct{}, maxParallel*buffersPerWorker),
	}
}

func (p *BoundedPool) AcquireBuffer(ctx context.Context) error {
	select {
	case p.buffers <- struct{}{}:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

func (p *BoundedPool) ReleaseBuffer() { <-p.buffers }

func (p *BoundedPool) Submit(ctx context.Context, task func(context.Context) error) error {
	if err := p.AcquireBuffer(ctx); err != nil {
		return err
	}
	select {
	case p.workers <- struct{}{}:
	case <-ctx.Done():
		p.ReleaseBuffer()
		return ctx.Err()
	}
	p.wg.Add(1)
	go func() {
		defer p.wg.Done()
		defer func() { <-p.workers }()
		defer p.ReleaseBuffer()
		_ = task(ctx)
	}()
	return nil
}

func (p *BoundedPool) Wait() { p.wg.Wait() }

type OrderedJob[T any] struct {
	ID    int
	Name  string
	Value T
}

// RunOrdered processes names in bounded parallel but returns results in the
// original order, eliminating map/goroutine scheduling from output bytes.
func RunOrdered[T any](
	ctx context.Context,
	names []string,
	params Params,
	run func(ctx context.Context, index int, name string) (T, error),
) ([]T, []error) {
	pool := NewBoundedPool(params.MaxParallelism, params.BufferMeshesPerWorker)
	results := make([]T, len(names))
	errsOut := make([]error, len(names))
	for i, name := range names {
		i, name := i, name
		err := pool.Submit(ctx, func(ctx context.Context) error {
			v, err := run(ctx, i, name)
			results[i] = v
			errsOut[i] = err
			return err
		})
		if err != nil {
			errsOut[i] = err
		}
	}
	pool.Wait()
	return results, errsOut
}
