package engine

import "cadlab/internal/sched"

func (e *Engine) LastQueue() sched.Metrics {
	e.mu.Lock()
	defer e.mu.Unlock()
	return e.queue
}
