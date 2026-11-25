package runner

import (
	"context"
	"sync"
)

type Gate struct {
	mu     sync.Mutex
	paused bool
	wait   chan struct{}
}

func NewGate() *Gate {
	return &Gate{wait: make(chan struct{})}
}

func (g *Gate) Pause() {
	g.mu.Lock()
	defer g.mu.Unlock()
	if !g.paused {
		g.paused = true
		g.wait = make(chan struct{})
	}
}

func (g *Gate) Resume() {
	g.mu.Lock()
	defer g.mu.Unlock()
	if g.paused {
		g.paused = false
		close(g.wait)
		g.wait = make(chan struct{})
	}
}

func (g *Gate) IsPaused() bool {
	g.mu.Lock()
	defer g.mu.Unlock()
	return g.paused
}

func (g *Gate) Check(ctx context.Context) error {
	if ctx != nil {
		select {
		case <-ctx.Done():
			return ctx.Err()
		default:
		}
	}
	g.mu.Lock()
	ch := g.wait
	paused := g.paused
	g.mu.Unlock()
	if !paused {
		return nil
	}
	select {
	case <-ch:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}
