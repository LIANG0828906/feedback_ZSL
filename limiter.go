package textureconv

import (
	"context"
	"sync"
	"time"
)

type BufferWait struct {
	Reason   string    `json:"reason"`
	Bytes    int       `json:"bytes"`
	At       time.Time `json:"at"`
	WaitedMS int64     `json:"waitedMs"`
}

// BufferLimiter bounds transient pixel buffers. Acquire blocks and records a
// reason instead of dropping work when the requested budget is unavailable.
type BufferLimiter struct {
	mu       sync.Mutex
	capacity int
	used     int
	notify   *sync.Cond
	waits    []BufferWait
}

func NewBufferLimiter(capacity int) *BufferLimiter {
	l := &BufferLimiter{capacity: capacity}
	l.notify = sync.NewCond(&l.mu)
	return l
}

func (l *BufferLimiter) Acquire(ctx context.Context, n int, reason string) error {
	if n <= 0 {
		return nil
	}
	start := time.Now()
	l.mu.Lock()
	defer l.mu.Unlock()
	stopWake := make(chan struct{})
	if ctx != nil {
		go func() {
			select {
			case <-ctx.Done():
				l.Wake()
			case <-stopWake:
			}
		}()
		defer close(stopWake)
	}
	recorded := false
	for l.used+n > l.capacity {
		if ctx != nil {
			select {
			case <-ctx.Done():
				return ctx.Err()
			default:
			}
		}
		if !recorded {
			l.waits = append(l.waits, BufferWait{Reason: reason, Bytes: n, At: start})
			recorded = true
		}
		l.notify.Wait()
	}
	l.used += n
	if recorded {
		l.waits[len(l.waits)-1].WaitedMS = time.Since(start).Milliseconds()
	}
	return nil
}

func (l *BufferLimiter) Release(n int) {
	if n <= 0 {
		return
	}
	l.mu.Lock()
	l.used -= n
	if l.used < 0 {
		l.used = 0
	}
	l.notify.Broadcast()
	l.mu.Unlock()
}

func (l *BufferLimiter) Wake() {
	l.mu.Lock()
	l.notify.Broadcast()
	l.mu.Unlock()
}

func (l *BufferLimiter) Used() int {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.used
}

func (l *BufferLimiter) Capacity() int { return l.capacity }

func (l *BufferLimiter) Waits() []BufferWait {
	l.mu.Lock()
	defer l.mu.Unlock()
	return append([]BufferWait(nil), l.waits...)
}
