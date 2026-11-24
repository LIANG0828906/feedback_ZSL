package pipeline

import (
	"context"
	"errors"
	"fmt"
	"sync"
)

var ErrBufferLimit = errors.New("unit exceeds per-worker pixel buffer limit")

type BufferPool struct {
	unitBytes int64
	capacity  int
	tokens    chan struct{}
	perWorker int64
}

func NewBufferPool(maxWorkers int, perWorkerBytes int64) *BufferPool {
	const unitBytes = int64(16 << 10)
	units := int((int64(maxWorkers) * perWorkerBytes) / unitBytes)
	if units < 1 {
		units = 1
	}
	return &BufferPool{
		unitBytes: unitBytes,
		capacity:  units,
		tokens:    make(chan struct{}, units),
		perWorker: perWorkerBytes,
	}
}

func (b *BufferPool) Acquire(ctx context.Context, bytes int64) (func(), error) {
	if bytes <= 0 {
		bytes = 1
	}
	if bytes > b.perWorker {
		return nil, fmt.Errorf("%w: requires %d bytes, limit is %d", ErrBufferLimit, bytes, b.perWorker)
	}
	units := int((bytes + b.unitBytes - 1) / b.unitBytes)
	if units > b.capacity {
		return nil, fmt.Errorf("%w: requires %d units, total capacity is %d", ErrBufferLimit, units, b.capacity)
	}
	acquired := 0
	for acquired < units {
		select {
		case b.tokens <- struct{}{}:
			acquired++
		case <-ctx.Done():
			for i := 0; i < acquired; i++ {
				<-b.tokens
			}
			return nil, ctx.Err()
		}
	}
	return func() {
		for i := 0; i < units; i++ {
			<-b.tokens
		}
	}, nil
}

// RunIndexed executes every unit exactly once. CPU concurrency is bounded by
// maxWorkers and pixel buffers are acquired from a shared weighted pool. A
// full buffer blocks the worker, leaving queued units pending instead of
// dropping them.
func RunIndexed(ctx context.Context, count, maxWorkers int, costs []int64, buffer *BufferPool, fn func(int) error) []error {
	results := make([]error, count)
	if count == 0 {
		return results
	}
	if maxWorkers < 1 {
		maxWorkers = 1
	}
	if maxWorkers > count {
		maxWorkers = count
	}
	jobs := make(chan int)
	var wg sync.WaitGroup
	for worker := 0; worker < maxWorkers; worker++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for index := range jobs {
				if err := ctx.Err(); err != nil {
					results[index] = err
					continue
				}
				cost := int64(1)
				if index < len(costs) && costs[index] > 0 {
					cost = costs[index]
				}
				release, err := buffer.Acquire(ctx, cost)
				if err != nil {
					results[index] = err
					continue
				}
				func() {
					defer release()
					defer func() {
						if recovered := recover(); recovered != nil {
							results[index] = fmt.Errorf("unit panic: %v", recovered)
						}
					}()
					results[index] = fn(index)
				}()
			}
		}()
	}
	for index := 0; index < count; index++ {
		select {
		case jobs <- index:
		case <-ctx.Done():
			// Fill unqueued units so results remain a complete matrix.
			for rest := index; rest < count; rest++ {
				results[rest] = ctx.Err()
			}
			close(jobs)
			wg.Wait()
			return results
		}
	}
	close(jobs)
	wg.Wait()
	return results
}
