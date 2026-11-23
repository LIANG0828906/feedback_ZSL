package pipeline

import (
	"context"
	"sync"

	"ocrworkbench/internal/domain"
)

type Control struct {
	mu           sync.Mutex
	paused       bool
	cancelled    bool
	params       domain.Params
	band         int
	paramVersion string
	autoPauseAt  int
	commitCount  int
}

func NewControl(params domain.Params, band int, paramVersion string) *Control {
	return &Control{params: params, band: band, paramVersion: paramVersion}
}

func (c *Control) Params() domain.Params {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.params
}

func (c *Control) SetAutoPauseAfter(commits int) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.autoPauseAt = commits
}

func (c *Control) Pause() {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.paused = true
}

func (c *Control) Resume() {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.paused = false
}

func (c *Control) Cancel() {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.cancelled = true
	c.paused = false
}

func (c *Control) UpdateParams(next domain.Params) *domain.AppError {
	c.mu.Lock()
	defer c.mu.Unlock()
	if appErr := next.Validate(); appErr != nil {
		return appErr
	}
	if c.paramVersion == "" {
		if next.StructuralVersion(0) != c.params.StructuralVersion(0) {
			e := domain.Errorf(domain.CodePara, "structural parameters cannot change before the run is initialized")
			e.Field = "structural_params"
			return e
		}
		c.params = next
		return nil
	}
	if next.StructuralVersion(c.band) != c.paramVersion {
		e := domain.Errorf(domain.CodePara, "structural parameters cannot change after a checkpoint exists")
		e.Field = "structural_params"
		e.Expected = c.paramVersion
		e.Actual = next.StructuralVersion(c.band)
		return e
	}
	c.params = next
	return nil
}

func (c *Control) MarkCommit() {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.commitCount++
	if c.autoPauseAt > 0 && c.commitCount >= c.autoPauseAt {
		c.paused = true
	}
}

func (c *Control) Checkpoint(ctx context.Context) (string, *domain.AppError) {
	select {
	case <-ctx.Done():
		return "cancelled", nil
	default:
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.cancelled {
		return "cancelled", nil
	}
	if c.paused {
		return "paused", nil
	}
	return "running", nil
}
