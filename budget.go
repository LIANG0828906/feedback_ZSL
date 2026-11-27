package volumefog

import (
	"sync"
	"time"
)

type BudgetWait struct {
	Chunk    ChunkCoord `json:"chunk"`
	Worker   int        `json:"worker"`
	Reason   string     `json:"reason"`
	WaitedUS int64      `json:"waited_us"`
	Needed   int        `json:"needed"`
	InUse    int        `json:"in_use"`
	Capacity int        `json:"capacity"`
}

type BudgetSnapshot struct {
	Capacity      int          `json:"capacity"`
	InUse         int          `json:"in_use"`
	Peak          int          `json:"peak"`
	Acquired      int          `json:"acquired"`
	WaitEvents    []BudgetWait `json:"wait_events"`
	WaitCount     int          `json:"wait_count"`
	DelayedChunks int          `json:"delayed_chunks"`
	TotalWaitUS   int64        `json:"total_wait_us"`
}

type Budget struct {
	mu          sync.Mutex
	cond        *sync.Cond
	capacity    int
	used        int
	peak        int
	acquired    int
	waits       []BudgetWait
	waitN       int
	totalWaitUS int64
	delayed     map[ChunkCoord]struct{}
}

func NewBudget(capacity int) *Budget {
	if capacity < 1 {
		capacity = 1
	}
	b := &Budget{capacity: capacity, delayed: map[ChunkCoord]struct{}{}}
	b.cond = sync.NewCond(&b.mu)
	return b
}

func (b *Budget) Capacity() int { return b.capacity }

func (b *Budget) AcquireShadow(n int, chunk ChunkCoord, worker int) func() {
	if n <= 0 || b == nil {
		return func() {}
	}
	if n > b.capacity {
		n = b.capacity
	}
	b.mu.Lock()
	start := time.Now()
	if b.used+n > b.capacity {
		b.delayed[chunk] = struct{}{}
		reason := "shadow sampling budget exhausted; delayed chunk queues for a later grant"
		for b.used+n > b.capacity {
			b.cond.Wait()
		}
		waited := time.Since(start)
		b.waitN++
		b.totalWaitUS += waited.Microseconds()
		if len(b.waits) < 1024 {
			b.waits = append(b.waits, BudgetWait{
				Chunk: chunk, Worker: worker, Reason: reason, WaitedUS: waited.Microseconds(),
				Needed: n, InUse: b.used, Capacity: b.capacity,
			})
		}
	}
	b.used += n
	b.acquired += n
	if b.used > b.peak {
		b.peak = b.used
	}
	b.mu.Unlock()
	var once sync.Once
	return func() {
		once.Do(func() {
			b.mu.Lock()
			b.used -= n
			if b.used < 0 {
				b.used = 0
			}
			b.cond.Broadcast()
			b.mu.Unlock()
		})
	}
}

func (b *Budget) RecordQueueWait(chunk ChunkCoord, worker int, d time.Duration, reason string) {
	if b == nil {
		return
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	b.waitN++
	b.totalWaitUS += d.Microseconds()
	b.delayed[chunk] = struct{}{}
	if len(b.waits) < 1024 {
		b.waits = append(b.waits, BudgetWait{
			Chunk: chunk, Worker: worker, Reason: reason, WaitedUS: d.Microseconds(),
			InUse: b.used, Capacity: b.capacity,
		})
	}
}

func (b *Budget) Snapshot() BudgetSnapshot {
	if b == nil {
		return BudgetSnapshot{}
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	out := BudgetSnapshot{
		Capacity: b.capacity, InUse: b.used, Peak: b.peak, Acquired: b.acquired,
		WaitCount: b.waitN, DelayedChunks: len(b.delayed), TotalWaitUS: b.totalWaitUS,
		WaitEvents: append([]BudgetWait(nil), b.waits...),
	}
	return out
}
