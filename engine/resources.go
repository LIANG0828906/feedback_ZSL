package engine

import (
	"context"
	"fmt"
	"sync"

	"virtualstudio/model"
)

type BufferEvent struct {
	Window int    `json:"window"`
	Worker int    `json:"worker"`
	Bytes  int64  `json:"bytes"`
	Reason string `json:"reason"`
	Queued bool   `json:"queued"`
}

type BufferLimiter struct {
	mu        sync.Mutex
	cond      *sync.Cond
	capacity  int64
	used      int64
	perWorker int64
	events    []BufferEvent
}

func NewBufferLimiter(maxWorkers int, perWorker int64) (*BufferLimiter, error) {
	return NewBufferLimiterWithTotal(maxWorkers, perWorker, 0)
}

func NewBufferLimiterWithTotal(maxWorkers int, perWorker, total int64) (*BufferLimiter, error) {
	if maxWorkers <= 0 {
		return nil, model.NewError(model.CodeParam, "max_workers", "must be > 0")
	}
	if perWorker <= 0 {
		return nil, model.NewError(model.CodeParam, "pixel_buffer_bytes_per_worker", "must be > 0")
	}
	if total <= 0 {
		total = int64(maxWorkers) * perWorker
	}
	l := &BufferLimiter{capacity: total, perWorker: perWorker}
	l.cond = sync.NewCond(&l.mu)
	return l, nil
}

func (l *BufferLimiter) Acquire(ctx context.Context, bytes int64, window, worker int) (func(), error) {
	if bytes <= 0 {
		return func() {}, nil
	}
	if bytes > l.perWorker {
		return nil, model.NewError(model.CodeParam, "pixel_buffer_bytes_per_worker",
			fmt.Sprintf("window %d requires %d bytes but per-worker limit is %d", window, bytes, l.perWorker))
	}
	if bytes > l.capacity {
		return nil, model.NewError(model.CodeParam, "pixel_buffer_total_bytes",
			fmt.Sprintf("window %d requires %d bytes but total budget is %d", window, bytes, l.capacity))
	}
	stop := make(chan struct{})
	go func() {
		select {
		case <-ctx.Done():
			l.mu.Lock()
			l.cond.Broadcast()
			l.mu.Unlock()
		case <-stop:
		}
	}()
	defer close(stop)
	l.mu.Lock()
	if l.used+bytes > l.capacity {
		l.events = append(l.events, BufferEvent{
			Window: window, Worker: worker, Bytes: bytes,
			Reason: "pixel buffer exhausted; queued until a worker releases capacity", Queued: true,
		})
	}
	for l.used+bytes > l.capacity {
		if err := ctx.Err(); err != nil {
			l.mu.Unlock()
			return nil, model.WrapError(model.CodeCancel, "pixel_buffer", "queued window canceled", err)
		}
		l.cond.Wait()
	}
	if err := ctx.Err(); err != nil {
		l.mu.Unlock()
		return nil, model.WrapError(model.CodeCancel, "pixel_buffer", "queued window canceled", err)
	}
	l.used += bytes
	l.mu.Unlock()
	var once sync.Once
	return func() {
		once.Do(func() {
			l.mu.Lock()
			l.used -= bytes
			l.cond.Broadcast()
			l.mu.Unlock()
		})
	}, nil
}

func (l *BufferLimiter) Events() []BufferEvent {
	l.mu.Lock()
	defer l.mu.Unlock()
	return append([]BufferEvent(nil), l.events...)
}
