package pipeline

import (
	"context"
	"fmt"
	"sync"
	"time"
)

type WaitEvent struct {
	Step     int           `json:"step"`
	Block    int           `json:"block"`
	Reason   string        `json:"reason"`
	Bytes    int64         `json:"bytes"`
	Duration time.Duration `json:"duration_ns"`
}

type BufferPool struct {
	mu       sync.Mutex
	cond     *sync.Cond
	capacity int64
	used     int64
	events   []WaitEvent
	minWait  time.Duration
}

func NewBufferPool(capacity int64) *BufferPool {
	b := &BufferPool{capacity: capacity, minWait: 50 * time.Microsecond}
	b.cond = sync.NewCond(&b.mu)
	return b
}

func (b *BufferPool) Acquire(ctx context.Context, bytes int64, step, block int, reason string) (func(), []WaitEvent, error) {
	if bytes <= 0 {
		return func() {}, nil, nil
	}
	if bytes > b.capacity {
		return nil, nil, fmt.Errorf("requested %d bytes exceeds buffer pool capacity %d", bytes, b.capacity)
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	start := time.Now()
	var waited bool
	done := make(chan struct{})
	go func() {
		select {
		case <-ctx.Done():
			b.cond.Broadcast()
		case <-done:
		}
	}()
	for b.used+bytes > b.capacity && ctx.Err() == nil {
		waited = true
		b.cond.Wait()
	}
	close(done)
	if err := ctx.Err(); err != nil {
		return nil, nil, err
	}
	b.used += bytes
	var events []WaitEvent
	if waited {
		d := time.Since(start)
		if d >= b.minWait {
			events = append(events, WaitEvent{
				Step: step, Block: block, Reason: reason,
				Bytes: bytes, Duration: d,
			})
			b.events = append(b.events, events...)
		}
	}
	release := func() {
		b.mu.Lock()
		b.used -= bytes
		b.cond.Broadcast()
		b.mu.Unlock()
	}
	return release, events, nil
}

func (b *BufferPool) Events() []WaitEvent {
	b.mu.Lock()
	defer b.mu.Unlock()
	out := make([]WaitEvent, len(b.events))
	copy(out, b.events)
	return out
}

func (b *BufferPool) Capacity() int64 { return b.capacity }
