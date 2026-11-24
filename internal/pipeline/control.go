package pipeline

import (
	"context"
	"errors"
	"sync"
	"time"
)

var ErrCanceled = errors.New("pipeline canceled")
var ErrPausedAbort = errors.New("pipeline paused and run aborted")
var ErrInjectedCrash = errors.New("injected checkpoint crash")

type Control struct {
	mu       sync.Mutex
	cond     *sync.Cond
	paused   bool
	canceled bool
}

func NewControl() *Control {
	c := &Control{}
	c.cond = sync.NewCond(&c.mu)
	return c
}

func (c *Control) Pause() {
	c.mu.Lock()
	c.paused = true
	c.mu.Unlock()
}

func (c *Control) Resume() {
	c.mu.Lock()
	c.paused = false
	c.cond.Broadcast()
	c.mu.Unlock()
}

func (c *Control) Cancel() {
	c.mu.Lock()
	c.canceled = true
	c.paused = false
	c.cond.Broadcast()
	c.mu.Unlock()
}

func (c *Control) Paused() bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.paused
}

func (c *Control) Canceled() bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.canceled
}

func (c *Control) Wait(ctx context.Context) error {
	if c == nil {
		return nil
	}
	for {
		c.mu.Lock()
		if c.canceled {
			c.mu.Unlock()
			return ErrCanceled
		}
		if !c.paused {
			c.mu.Unlock()
			return ctx.Err()
		}
		c.mu.Unlock()
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(50 * time.Millisecond):
		}
	}
}
