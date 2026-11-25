package engine

import (
	"context"
	"fmt"
	"sync"

	"uvenhance/internal/apperr"
)

type BufferEvent struct {
	Stage    string `json:"stage"`
	Reason   string `json:"reason"`
	Need     int64  `json:"need_bytes"`
	Used     int64  `json:"used_bytes"`
	Capacity int64  `json:"capacity_bytes"`
}

type bufferLimiter struct {
	mu       sync.Mutex
	capacity int64
	used     int64
	notify   chan struct{}
	events   []BufferEvent
}

func newBufferLimiter(capacity int64) *bufferLimiter {
	return &bufferLimiter{capacity: capacity, notify: make(chan struct{})}
}

func (b *bufferLimiter) acquire(ctx context.Context, stage string, need int64) (func(), error) {
	logged := false
	for {
		b.mu.Lock()
		if need <= b.capacity-b.used {
			b.used += need
			b.mu.Unlock()
			return func() { b.release(need) }, nil
		}
		if !logged {
			b.events = append(b.events, BufferEvent{
				Stage: stage, Reason: "buffer_exhausted_waiting",
				Need: need, Used: b.used, Capacity: b.capacity,
			})
			logged = true
		}
		wait := b.notify
		b.mu.Unlock()
		select {
		case <-ctx.Done():
			return nil, apperr.Canceled(ctx.Err())
		case <-wait:
		}
	}
}

func (b *bufferLimiter) release(need int64) {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.used -= need
	if b.used < 0 {
		b.used = 0
	}
	close(b.notify)
	b.notify = make(chan struct{})
}

func (b *bufferLimiter) snapshotEvents() []BufferEvent {
	b.mu.Lock()
	defer b.mu.Unlock()
	out := append([]BufferEvent(nil), b.events...)
	return out
}

func (b *bufferLimiter) record(event BufferEvent) {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.events = append(b.events, event)
}

func (b *bufferLimiter) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return fmt.Sprintf("used=%d capacity=%d waits=%d", b.used, b.capacity, len(b.events))
}
