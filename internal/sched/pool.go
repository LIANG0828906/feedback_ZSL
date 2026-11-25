package sched

import (
	"context"
	"sync"
	"sync/atomic"
	"time"
)

type WaitRecord struct {
	Reason   string `json:"reason"`
	Worker   int    `json:"worker"`
	WaitMS   int64  `json:"wait_ms"`
	Buffered int    `json:"buffered"`
}

type Metrics struct {
	MaxWorkers      int          `json:"max_workers"`
	BufferPerWorker int          `json:"buffer_per_worker"`
	Submitted       int64        `json:"submitted"`
	Completed       int64        `json:"completed"`
	WaitEvents      int64        `json:"wait_events"`
	TotalWaitMS     int64        `json:"total_wait_ms"`
	MaxWaitMS       int64        `json:"max_wait_ms"`
	Records         []WaitRecord `json:"records"`
}

type job struct {
	fn   func() error
	done chan struct{}
	err  error
}

type Future struct {
	j *job
}

func (f Future) Wait() error {
	<-f.j.done
	return f.j.err
}

type Pool struct {
	maxWorkers      int
	bufferPerWorker int
	queues          []chan *job
	wg              sync.WaitGroup
	rr              uint64
	stop            chan struct{}
	stopOnce        sync.Once

	mu      sync.Mutex
	metrics Metrics
	closed  bool
}

func NewPool(maxWorkers, bufferPerWorker int) *Pool {
	if maxWorkers < 1 {
		maxWorkers = 1
	}
	if bufferPerWorker < 1 {
		bufferPerWorker = 1
	}
	p := &Pool{
		maxWorkers:      maxWorkers,
		bufferPerWorker: bufferPerWorker,
		queues:          make([]chan *job, maxWorkers),
		stop:            make(chan struct{}),
	}
	p.metrics.MaxWorkers = maxWorkers
	p.metrics.BufferPerWorker = bufferPerWorker
	for i := 0; i < maxWorkers; i++ {
		p.queues[i] = make(chan *job, bufferPerWorker)
		p.wg.Add(1)
		go p.worker(i, p.queues[i])
	}
	return p
}

func (p *Pool) worker(id int, ch <-chan *job) {
	defer p.wg.Done()
	for {
		select {
		case <-p.stop:
			return
		case j := <-ch:
			if j == nil {
				return
			}
			j.err = j.fn()
			atomic.AddInt64(&p.metrics.Completed, 1)
			close(j.done)
		}
	}
}

func (p *Pool) Submit(ctx context.Context, reason string, fn func() error) (Future, error) {
	p.mu.Lock()
	if p.closed {
		p.mu.Unlock()
		return Future{}, ErrPoolClosed
	}
	p.mu.Unlock()

	idx := p.pickWorker()
	j := &job{fn: fn, done: make(chan struct{})}
	start := time.Now()

	select {
	case p.queues[idx] <- j:
	case <-ctx.Done():
		return Future{}, ctx.Err()
	}
	waited := time.Since(start).Milliseconds()
	atomic.AddInt64(&p.metrics.Submitted, 1)
	if waited > 0 {
		p.mu.Lock()
		p.metrics.WaitEvents++
		p.metrics.TotalWaitMS += waited
		if waited > p.metrics.MaxWaitMS {
			p.metrics.MaxWaitMS = waited
		}
		p.metrics.Records = append(p.metrics.Records, WaitRecord{Reason: reason, Worker: idx, WaitMS: waited, Buffered: len(p.queues[idx])})
		p.mu.Unlock()
	}
	return Future{j: j}, nil
}

func (p *Pool) pickWorker() int {
	best := 0
	bestLen := len(p.queues[0])
	for i := 1; i < len(p.queues); i++ {
		if n := len(p.queues[i]); n < bestLen {
			best = i
			bestLen = n
		}
	}
	if bestLen < cap(p.queues[best]) {
		return best
	}
	return int(atomic.AddUint64(&p.rr, 1)-1) % p.maxWorkers
}

func (p *Pool) Close() {
	p.mu.Lock()
	if p.closed {
		p.mu.Unlock()
		return
	}
	p.closed = true
	p.mu.Unlock()
	p.stopOnce.Do(func() { close(p.stop) })
	p.wg.Wait()
}

func (p *Pool) Metrics() Metrics {
	p.mu.Lock()
	defer p.mu.Unlock()
	records := append([]WaitRecord(nil), p.metrics.Records...)
	return Metrics{
		MaxWorkers:      p.maxWorkers,
		BufferPerWorker: p.bufferPerWorker,
		Submitted:       atomic.LoadInt64(&p.metrics.Submitted),
		Completed:       atomic.LoadInt64(&p.metrics.Completed),
		WaitEvents:      p.metrics.WaitEvents,
		TotalWaitMS:     p.metrics.TotalWaitMS,
		MaxWaitMS:       p.metrics.MaxWaitMS,
		Records:         records,
	}
}

type poolError string

func (e poolError) Error() string { return string(e) }

const ErrPoolClosed = poolError("scheduler pool is closed")
