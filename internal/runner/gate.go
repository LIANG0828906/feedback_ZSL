package runner

import (
	"context"
	"sync"
	"time"

	"channelpipe"
)

type BufferWait struct {
	Reason     string `json:"reason"`
	CostPixels int    `json:"cost_pixels"`
	WaitMS     int64  `json:"wait_ms"`
}

type BufferGate struct {
	mu             sync.Mutex
	cond           *sync.Cond
	capacityPixels int
	perTaskLimit   int
	usedPixels     int
	Waits          []BufferWait
}

func NewBufferGate(maxWorkers, perThreadPixels int) *BufferGate {
	if maxWorkers <= 0 {
		maxWorkers = 1
	}
	if perThreadPixels <= 0 {
		perThreadPixels = 1 << 20
	}
	gate := &BufferGate{capacityPixels: maxWorkers * perThreadPixels, perTaskLimit: perThreadPixels}
	gate.cond = sync.NewCond(&gate.mu)
	return gate
}

func (g *BufferGate) PerTaskLimit() int { return g.perTaskLimit }

func (g *BufferGate) Acquire(ctx context.Context, cost int, reason string) (int64, error) {
	if cost > g.perTaskLimit {
		return 0, channelpipe.FieldError(channelpipe.CodeInvalid, "buffer_budget", "chunk buffer cost %d exceeds per-thread limit %d", cost, g.perTaskLimit)
	}
	start := time.Now()
	g.mu.Lock()
	defer g.mu.Unlock()

	stopWatcher := make(chan struct{})
	go func() {
		select {
		case <-ctx.Done():
			g.mu.Lock()
			g.cond.Broadcast()
			g.mu.Unlock()
		case <-stopWatcher:
		}
	}()
	defer close(stopWatcher)

	for g.usedPixels+cost > g.capacityPixels {
		g.cond.Wait()
		if err := ctx.Err(); err != nil {
			waitMS := time.Since(start).Milliseconds()
			g.Waits = append(g.Waits, BufferWait{Reason: reason, CostPixels: cost, WaitMS: waitMS})
			return waitMS, err
		}
	}
	g.usedPixels += cost
	waitMS := time.Since(start).Milliseconds()
	g.Waits = append(g.Waits, BufferWait{Reason: reason, CostPixels: cost, WaitMS: waitMS})
	return waitMS, nil
}

func (g *BufferGate) Release(cost int) {
	g.mu.Lock()
	defer g.mu.Unlock()
	g.usedPixels -= cost
	if g.usedPixels < 0 {
		g.usedPixels = 0
	}
	g.cond.Broadcast()
}

func (g *BufferGate) SnapshotWaits() []BufferWait {
	g.mu.Lock()
	defer g.mu.Unlock()
	return append([]BufferWait(nil), g.Waits...)
}
