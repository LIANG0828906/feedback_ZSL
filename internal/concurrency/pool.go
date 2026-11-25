package concurrency

import (
	"context"
	"sync"
	"time"

	"sre-timeline/internal/bizerr"
)

type Config struct {
	Workers     int `json:"workers"`
	BufferLimit int `json:"buffer_limit"`
}

func (c Config) Normalize() Config {
	if c.Workers <= 0 {
		c.Workers = 4
	}
	if c.BufferLimit <= 0 {
		c.BufferLimit = 4096
	}
	return c
}

// ServiceBuffer is a fixed-capacity per-service ingestion buffer. It never
// grows implicitly and never silently drops an event.
type ServiceBuffer[T any] struct {
	ServiceID string
	Limit     int
	ch        chan T
}

func NewServiceBuffer[T any](serviceID string, limit int) (*ServiceBuffer[T], error) {
	if limit <= 0 {
		return nil, bizerr.New(bizerr.CodeBackpressureBuffer, "buffer.limit", "buffer limit must be positive").
			With("service", serviceID).With("limit", limit)
	}
	return &ServiceBuffer[T]{ServiceID: serviceID, Limit: limit, ch: make(chan T, limit)}, nil
}

func (b *ServiceBuffer[T]) TryPush(value T) error {
	select {
	case b.ch <- value:
		return nil
	default:
		return bizerr.New(bizerr.CodeBackpressureBuffer, "service_buffer", "per-service event buffer is full").
			With("service", b.ServiceID).With("limit", b.Limit)
	}
}

func (b *ServiceBuffer[T]) Items() []T {
	close(b.ch)
	out := make([]T, 0, len(b.ch))
	for item := range b.ch {
		out = append(out, item)
	}
	return out
}

// Pool has fixed workers and a bounded FIFO job queue.
type Pool struct {
	workers int
	jobs    chan func()
	wg      sync.WaitGroup
}

func NewPool(cfg Config) (*Pool, error) {
	cfg = cfg.Normalize()
	if cfg.Workers <= 0 {
		return nil, bizerr.New(bizerr.CodeBackpressureWorker, "workers", "worker budget must be positive").With("workers", cfg.Workers)
	}
	p := &Pool{workers: cfg.Workers, jobs: make(chan func(), cfg.BufferLimit)}
	for i := 0; i < cfg.Workers; i++ {
		p.wg.Add(1)
		go func() {
			defer p.wg.Done()
			for fn := range p.jobs {
				fn()
			}
		}()
	}
	return p, nil
}

func (p *Pool) Submit(ctx context.Context, fn func()) error {
	select {
	case p.jobs <- fn:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

// TrySubmit returns a stable backpressure code when the bounded queue is full.
func (p *Pool) TrySubmit(fn func()) error {
	select {
	case p.jobs <- fn:
		return nil
	default:
		return bizerr.New(bizerr.CodeBackpressureWorker, "worker_pool.queue", "worker budget and bounded queue are saturated").
			With("workers", p.workers).With("queue_capacity", cap(p.jobs))
	}
}

func (p *Pool) Close() {
	close(p.jobs)
	p.wg.Wait()
}

// RunOrdered runs one item per key in parallel and returns results indexed by
// the original keys. Callers sort before producing user-facing output.
func RunOrdered[K comparable, T any](ctx context.Context, cfg Config, keys []K, fn func(K) (T, error)) (map[K]T, map[K]int64, error) {
	cfg = cfg.Normalize()
	results := map[K]T{}
	durations := map[K]int64{}
	var mu sync.Mutex
	var firstErr error
	var errOnce sync.Once

	sem := make(chan struct{}, cfg.Workers)
	var wg sync.WaitGroup
	for _, key := range keys {
		key := key
		select {
		case <-ctx.Done():
			wg.Wait()
			return nil, nil, ctx.Err()
		case sem <- struct{}{}:
		}
		wg.Add(1)
		go func() {
			defer wg.Done()
			defer func() { <-sem }()
			start := nowNano()
			value, err := fn(key)
			elapsed := nowNano() - start
			mu.Lock()
			durations[key] = elapsed
			if err != nil {
				errOnce.Do(func() { firstErr = err })
			} else {
				results[key] = value
			}
			mu.Unlock()
		}()
	}
	wg.Wait()
	if firstErr != nil {
		return nil, durations, firstErr
	}
	return results, durations, nil
}

func nowNano() int64 { return time.Now().UnixNano() }
