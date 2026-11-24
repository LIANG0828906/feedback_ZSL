package session

import (
	"context"
	"sync"
	"sync/atomic"
)

type Job func(context.Context) error

type PoolStats struct {
	Submitted   int64 `json:"submitted"`
	Completed   int64 `json:"completed"`
	Failed      int64 `json:"failed"`
	Blocked     int64 `json:"blocked_submits"`
	InFlight    int64 `json:"in_flight"`
	MaxInFlight int64 `json:"max_in_flight"`
	Buffered    int64 `json:"buffered"`
}

type Pool struct {
	queues   []chan Job
	wg       sync.WaitGroup
	ctx      context.Context
	cancel   context.CancelFunc
	submitMu sync.RWMutex
	next     atomic.Uint64
	stats    PoolStats
	statsMu  sync.Mutex
	closed   atomic.Bool
}

func NewPool(ctx context.Context, maxWorkers, perWorkerBuffer int) *Pool {
	if maxWorkers <= 0 {
		maxWorkers = 1
	}
	if perWorkerBuffer < 0 {
		perWorkerBuffer = 0
	}
	poolCtx, cancel := context.WithCancel(ctx)
	p := &Pool{
		queues: make([]chan Job, maxWorkers),
		ctx:    poolCtx,
		cancel: cancel,
	}
	for i := 0; i < maxWorkers; i++ {
		p.queues[i] = make(chan Job, perWorkerBuffer)
		p.wg.Add(1)
		go p.worker(p.queues[i])
	}
	return p
}

func (p *Pool) worker(queue <-chan Job) {
	defer p.wg.Done()
	for {
		select {
		case <-p.ctx.Done():
			return
		case job, ok := <-queue:
			if !ok {
				return
			}
			if job == nil {
				continue
			}
			p.add(func(s *PoolStats) {
				s.InFlight++
				if s.InFlight > s.MaxInFlight {
					s.MaxInFlight = s.InFlight
				}
			})
			err := job(p.ctx)
			p.add(func(s *PoolStats) {
				s.InFlight--
				s.Completed++
				if err != nil {
					s.Failed++
				}
			})
		}
	}
}

// Submit blocks when the selected worker's bounded event buffer is full. It
// never drops work; cancellation is the only reason a job is not accepted.
func (p *Pool) Submit(ctx context.Context, job Job) error {
	p.submitMu.RLock()
	defer p.submitMu.RUnlock()
	if p.closed.Load() {
		return context.Canceled
	}
	if ctx == nil {
		ctx = context.Background()
	}
	idx := int(p.next.Add(1)-1) % len(p.queues)
	queue := p.queues[idx]
	if len(queue) == cap(queue) {
		p.add(func(s *PoolStats) { s.Blocked++ })
	}
	p.add(func(s *PoolStats) { s.Submitted++ })
	select {
	case queue <- job:
		p.add(func(s *PoolStats) { s.Buffered++ })
		return nil
	case <-ctx.Done():
		return ctx.Err()
	case <-p.ctx.Done():
		return context.Canceled
	}
}

func (p *Pool) CloseAndWait() {
	p.submitMu.Lock()
	if p.closed.Swap(true) {
		p.submitMu.Unlock()
		return
	}
	for _, queue := range p.queues {
		close(queue)
	}
	p.submitMu.Unlock()
	p.wg.Wait()
	p.cancel()
}

func (p *Pool) Stats() PoolStats {
	p.statsMu.Lock()
	defer p.statsMu.Unlock()
	stats := p.stats
	for _, queue := range p.queues {
		stats.Buffered -= int64(cap(queue) - len(queue))
	}
	// Buffered tracked as cumulative accepted; current queue occupancy is more
	// useful to callers, so recompute it explicitly.
	stats.Buffered = 0
	for _, queue := range p.queues {
		stats.Buffered += int64(len(queue))
	}
	return stats
}

func (p *Pool) add(fn func(*PoolStats)) {
	p.statsMu.Lock()
	fn(&p.stats)
	p.statsMu.Unlock()
}
