package pipeline

import (
	"context"
	"sync"

	"qualitybench/internal/core"
)

type Control struct {
	mu     sync.Mutex
	paused bool
	resume chan struct{}
	ctx    context.Context
	cancel context.CancelFunc
}

func NewControl(parent context.Context) *Control {
	ctx, cancel := context.WithCancel(parent)
	c := &Control{ctx: ctx, cancel: cancel, resume: make(chan struct{})}
	close(c.resume)
	return c
}

func (c *Control) Context() context.Context { return c.ctx }

func (c *Control) Wait(ctx context.Context) error {
	for {
		c.mu.Lock()
		paused := c.paused
		resume := c.resume
		c.mu.Unlock()
		if !paused {
			select {
			case <-ctx.Done():
				return ctx.Err()
			case <-c.ctx.Done():
				return c.ctx.Err()
			default:
				return nil
			}
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-c.ctx.Done():
			return c.ctx.Err()
		case <-resume:
		}
	}
}

func (c *Control) Pause() {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.paused {
		return
	}
	c.paused = true
	c.resume = make(chan struct{})
}

func (c *Control) Resume() {
	c.mu.Lock()
	defer c.mu.Unlock()
	if !c.paused {
		return
	}
	c.paused = false
	close(c.resume)
}

func (c *Control) Cancel() {
	c.mu.Lock()
	if c.paused {
		c.paused = false
		close(c.resume)
	}
	c.mu.Unlock()
	c.cancel()
}

func (c *Control) Canceled() bool {
	select {
	case <-c.ctx.Done():
		return true
	default:
		return false
	}
}

func (c *Control) Paused() bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.paused
}

func canceledError() error {
	return core.CanceledError("operation canceled")
}
