package stage

import (
	"context"
	"fmt"
	"sync"
	"time"
)

type BudgetRecord struct {
	At         time.Time `json:"at"`
	Window     int       `json:"window"`
	Event      string    `json:"event"`
	Required   int64     `json:"required_bytes"`
	Available  int64     `json:"available_bytes"`
	Max        int64     `json:"max_bytes"`
	WaitMillis int64     `json:"wait_ms"`
	Reason     string    `json:"reason"`
}

type memoryBudget struct {
	mu      sync.Mutex
	max     int64
	used    int64
	wait    chan struct{}
	waiters map[int]struct{}
	records []BudgetRecord
}

func newMemoryBudget(max int64) *memoryBudget {
	return &memoryBudget{max: max, wait: make(chan struct{}), waiters: make(map[int]struct{})}
}

func (b *memoryBudget) Acquire(ctx context.Context, window int, required int64) (int64, error) {
	if required <= 0 {
		return 0, nil
	}
	if required > b.max {
		b.Record(BudgetRecord{
			Window: window, Event: "rejected", Required: required, Available: b.available(), Max: b.max,
			Reason: "window buffer requirement exceeds total memory budget",
		})
		err := newError(CodeState, "window exceeds shared memory budget", fmt.Sprintf("window:%d", window), "memory_budget")
		return 0, withDetail(err, ErrorDetail{Expected: fmt.Sprintf("<=%d", b.max), Actual: fmt.Sprint(required), Window: window})
	}
	start := time.Now()
	blocked := false
	b.mu.Lock()
	b.waiters[window] = struct{}{}
	b.mu.Unlock()
	for {
		b.mu.Lock()
		earlierWaiter := b.hasEarlierWaiterLocked(window)
		if !earlierWaiter && b.used+required <= b.max {
			b.used += required
			available := b.max - b.used
			delete(b.waiters, window)
			b.notifyLocked()
			b.mu.Unlock()
			if blocked {
				b.Record(BudgetRecord{
					Window: window, Event: "acquired", Required: required, Available: available, Max: b.max,
					WaitMillis: time.Since(start).Milliseconds(),
					Reason:     "admitted after budget became available",
				})
			}
			return required, nil
		}
		available := b.max - b.used
		wait := b.wait
		b.mu.Unlock()
		if !blocked {
			blocked = true
			reason := "insufficient memory budget; queued behind in-flight windows"
			if earlierWaiter {
				reason = "queued behind an earlier uncommitted window to preserve commit order"
			}
			b.Record(BudgetRecord{
				Window: window, Event: "blocked", Required: required, Available: available, Max: b.max,
				Reason: reason,
			})
		}
		select {
		case <-ctx.Done():
			b.mu.Lock()
			delete(b.waiters, window)
			b.mu.Unlock()
			b.Record(BudgetRecord{
				Window: window, Event: "cancelled", Required: required, Available: available, Max: b.max,
				WaitMillis: time.Since(start).Milliseconds(),
				Reason:     "cancelled while waiting for memory budget",
			})
			return 0, ctx.Err()
		case <-wait:
		}
	}
}

func (b *memoryBudget) Release(bytes int64) {
	if bytes <= 0 {
		return
	}
	b.mu.Lock()
	b.used -= bytes
	if b.used < 0 {
		b.used = 0
	}
	b.notifyLocked()
	b.mu.Unlock()
}

func (b *memoryBudget) notifyLocked() {
	close(b.wait)
	b.wait = make(chan struct{})
}

func (b *memoryBudget) Record(record BudgetRecord) {
	b.mu.Lock()
	defer b.mu.Unlock()
	if record.At.IsZero() {
		record.At = time.Now().UTC()
	}
	b.records = append(b.records, record)
}

func (b *memoryBudget) Records() []BudgetRecord {
	b.mu.Lock()
	defer b.mu.Unlock()
	out := make([]BudgetRecord, len(b.records))
	copy(out, b.records)
	return out
}

func (b *memoryBudget) available() int64 {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.max - b.used
}

func (b *memoryBudget) hasEarlierWaiterLocked(window int) bool {
	for pending := range b.waiters {
		if pending < window {
			return true
		}
	}
	return false
}
