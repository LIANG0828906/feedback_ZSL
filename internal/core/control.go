package core

import (
	"context"
	"sync"
	"time"
)

type RunControl struct {
	mu     sync.Mutex
	paused bool
}

func NewRunControl() *RunControl { return &RunControl{} }

func (c *RunControl) SetPaused(paused bool) {
	c.mu.Lock()
	c.paused = paused
	c.mu.Unlock()
}

func (c *RunControl) Paused() bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.paused
}

func (c *RunControl) Wait(ctx context.Context) error {
	if c == nil {
		return ctx.Err()
	}
	for c.Paused() {
		select {
		case <-ctx.Done():
			return WrapError(CodeCanceled, "", "run canceled while paused", ctx.Err())
		case <-time.After(15 * time.Millisecond):
		}
	}
	if err := ctx.Err(); err != nil {
		return WrapError(CodeCanceled, "", "run canceled", err)
	}
	return nil
}
