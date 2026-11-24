package pipeline

import (
	"context"
	"sync"
)

// PoolEvent 记录缓冲耗尽导致的阻塞排队事件（不静默丢弃任何任务）。
type PoolEvent struct {
	Reason string `json:"reason"`
	Frame  int    `json:"frame"`
}

// Pool 是受限工作线程池：maxWorkers 控制最大并行度，
// bufferCap 以“采样点数预算”限制在途缓冲，预算耗尽时提交方阻塞排队。
type Pool struct {
	sem       chan struct{}
	budgetMu  sync.Mutex
	budgetCV  *sync.Cond
	budget    int
	budgetCap int
	events    []PoolEvent
	eventMu   sync.Mutex
}

func NewPool(maxWorkers, bufferCap int) *Pool {
	p := &Pool{
		sem:       make(chan struct{}, maxWorkers),
		budget:    bufferCap,
		budgetCap: bufferCap,
	}
	p.budgetCV = sync.NewCond(&p.budgetMu)
	return p
}

// Acquire 占用一个 worker 与 frameSamples 个采样缓冲；预算不足时阻塞并记录原因。
func (p *Pool) Acquire(ctx context.Context, frame, frameSamples int) error {
	done := make(chan struct{})
	defer close(done)
	go func() {
		select {
		case <-ctx.Done():
			p.budgetCV.Broadcast()
		case <-done:
		}
	}()

	p.budgetMu.Lock()
	blocked := false
	for p.budget < frameSamples {
		if !blocked {
			p.record(PoolEvent{Reason: "buffer_exhausted_blocked", Frame: frame})
			blocked = true
		}
		if err := ctx.Err(); err != nil {
			p.budgetMu.Unlock()
			return err
		}
		p.budgetCV.Wait()
	}
	p.budget -= frameSamples
	p.budgetMu.Unlock()

	select {
	case p.sem <- struct{}{}:
		return nil
	case <-ctx.Done():
		p.Release(frameSamples)
		return ctx.Err()
	}
}

func (p *Pool) Release(frameSamples int) {
	<-p.sem
	p.budgetMu.Lock()
	p.budget += frameSamples
	if p.budget > p.budgetCap {
		p.budget = p.budgetCap
	}
	p.budgetMu.Unlock()
	p.budgetCV.Broadcast()
}

func (p *Pool) record(ev PoolEvent) {
	p.eventMu.Lock()
	p.events = append(p.events, ev)
	p.eventMu.Unlock()
}

func (p *Pool) Events() []PoolEvent {
	p.eventMu.Lock()
	defer p.eventMu.Unlock()
	out := make([]PoolEvent, len(p.events))
	copy(out, p.events)
	return out
}
