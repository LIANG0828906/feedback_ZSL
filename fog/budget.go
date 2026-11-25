package fog

import (
	"context"
	"sync"
	"sync/atomic"
	"time"
)

type QueueEvent struct {
	TileID   string  `json:"tile_id"`
	Reason   string  `json:"reason"`
	WaitMS   float64 `json:"wait_ms"`
	Blocked  bool    `json:"blocked"`
	Units    int     `json:"units"`
	Count    int     `json:"count"`
	Sequence int64   `json:"sequence"`
}

type BudgetStats struct {
	Capacity      int     `json:"capacity"`
	PeakOccupancy int     `json:"peak_occupancy"`
	AcquiredTotal int64   `json:"acquired_total"`
	WaitCount     int64   `json:"wait_count"`
	WaitTotalMS   float64 `json:"wait_total_ms"`
	Clamped       int64   `json:"clamped_requests"`
}

// shadowBudget is a counting semaphore. Holding a token means a shadow sample
// is in flight. When the budget is full, Acquire blocks instead of dropping the
// ray; every waited acquisition is recorded for diagnostics.
type shadowBudget struct {
	tokens chan struct{}
	cap    int

	mu        sync.Mutex
	events    []QueueEvent
	seq       int64
	peak      int
	acquired  int64
	waitCount int64
	waitMS    float64
	clamped   int64
}

func newShadowBudget(capacity int) *shadowBudget {
	if capacity < 1 {
		capacity = 1
	}
	return &shadowBudget{tokens: make(chan struct{}, capacity), cap: capacity}
}

func (b *shadowBudget) acquire(ctx context.Context, requested int, tileID, rayID string) (int, error) {
	if requested <= 0 {
		return 0, nil
	}
	actual := requested
	if actual > b.cap {
		actual = b.cap
		b.mu.Lock()
		b.clamped++
		b.seq++
		b.appendEventLocked(QueueEvent{
			TileID: tileID, Reason: "shadow-request-clamped", Units: actual,
			Count: 1, Sequence: b.seq,
		})
		b.mu.Unlock()
	}
	start := time.Now()
	got := 0
	blocked := false
	for got < actual {
		if !blocked {
			select {
			case b.tokens <- struct{}{}:
				got++
				now := len(b.tokens)
				b.mu.Lock()
				if now > b.peak {
					b.peak = now
				}
				b.mu.Unlock()
				continue
			default:
				blocked = true
			}
		}
		select {
		case b.tokens <- struct{}{}:
			got++
			now := len(b.tokens)
			b.mu.Lock()
			if now > b.peak {
				b.peak = now
			}
			b.mu.Unlock()
		case <-ctx.Done():
			for i := 0; i < got; i++ {
				<-b.tokens
			}
			return got, ctx.Err()
		}
	}
	wait := time.Since(start)
	b.mu.Lock()
	b.acquired += int64(actual)
	if blocked {
		b.waitCount++
		b.waitMS += float64(wait) / float64(time.Millisecond)
		b.seq++
		b.appendEventLocked(QueueEvent{
			TileID: tileID, Reason: "shadow-sample-budget", WaitMS: float64(wait) / float64(time.Millisecond),
			Blocked: true, Units: actual, Count: 1, Sequence: b.seq,
		})
	}
	b.mu.Unlock()
	_ = rayID
	return actual, nil
}

func (b *shadowBudget) appendEventLocked(e QueueEvent) {
	for i := range b.events {
		if b.events[i].TileID == e.TileID && b.events[i].Reason == e.Reason {
			b.events[i].WaitMS += e.WaitMS
			b.events[i].Units += e.Units
			b.events[i].Count++
			return
		}
	}
	b.events = append(b.events, e)
}

func (b *shadowBudget) release(n int) {
	for i := 0; i < n; i++ {
		select {
		case <-b.tokens:
		default:
			return
		}
	}
}

func (b *shadowBudget) stats() BudgetStats {
	b.mu.Lock()
	defer b.mu.Unlock()
	return BudgetStats{
		Capacity: b.cap, PeakOccupancy: b.peak, AcquiredTotal: b.acquired,
		WaitCount: b.waitCount, WaitTotalMS: b.waitMS, Clamped: b.clamped,
	}
}

func (b *shadowBudget) queueEvents() []QueueEvent {
	b.mu.Lock()
	defer b.mu.Unlock()
	return append([]QueueEvent(nil), b.events...)
}

// blockingCounter is used for worker-slot admission. It uses a buffered
// channel so cancellation remains possible while queued.
type blockingCounter struct {
	tokens chan struct{}
	cap    int

	mu       sync.Mutex
	events   []QueueEvent
	seq      int64
	peak     int32
	acquired int64
}

func newBlockingCounter(capacity int) *blockingCounter {
	if capacity < 1 {
		capacity = 1
	}
	return &blockingCounter{tokens: make(chan struct{}, capacity), cap: capacity}
}

func (c *blockingCounter) acquire(ctx context.Context, tileID string) (time.Duration, error) {
	start := time.Now()
	blocked := false
	select {
	case c.tokens <- struct{}{}:
	default:
		blocked = true
		select {
		case c.tokens <- struct{}{}:
		case <-ctx.Done():
			return time.Since(start), ctx.Err()
		}
	}
	wait := time.Since(start)
	now := int32(len(c.tokens))
	for {
		old := atomic.LoadInt32(&c.peak)
		if now <= old || atomic.CompareAndSwapInt32(&c.peak, old, now) {
			break
		}
	}
	c.mu.Lock()
	c.acquired++
	if blocked {
		c.seq++
		c.events = append(c.events, QueueEvent{
			TileID: tileID, Reason: "max-parallel-tile-slot",
			WaitMS: float64(wait) / float64(time.Millisecond), Blocked: true, Units: 1, Count: 1, Sequence: c.seq,
		})
	}
	c.mu.Unlock()
	return wait, nil
}

func (c *blockingCounter) release() { <-c.tokens }

func (c *blockingCounter) eventsSnapshot() []QueueEvent {
	c.mu.Lock()
	defer c.mu.Unlock()
	return append([]QueueEvent(nil), c.events...)
}
