package scheduler

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"sync/atomic"
	"time"
)

type Config struct {
	MaxWorkers   int
	WorkerBuffer int
	QueueBudget  int
}

type WaitRecord struct {
	TaskID   string `json:"task_id"`
	Reason   string `json:"reason"`
	WaitedMS int64  `json:"waited_ms"`
	AtUnixMS int64  `json:"at_unix_ms"`
}

type Stats struct {
	MaxConcurrency int32        `json:"max_concurrency"`
	Submitted      int64        `json:"submitted"`
	Completed      int64        `json:"completed"`
	InFlight       int64        `json:"in_flight"`
	WaitCount      int          `json:"wait_count"`
	Waits          []WaitRecord `json:"waits"`
}

type job struct {
	ctx  context.Context
	id   string
	fn   func(context.Context) error
	done chan error
}

type Pool struct {
	cfg       Config
	mailboxes []chan job
	budget    chan struct{}
	rr        uint64
	wg        sync.WaitGroup

	concurrency int32
	maxConc     int32
	submitted   int64
	completed   int64
	inFlight    int64

	mu    sync.Mutex
	waits []WaitRecord
}

func New(cfg Config) *Pool {
	if cfg.MaxWorkers <= 0 {
		cfg.MaxWorkers = 2
	}
	if cfg.WorkerBuffer <= 0 {
		cfg.WorkerBuffer = 4
	}
	if cfg.QueueBudget <= 0 {
		cfg.QueueBudget = cfg.MaxWorkers * (cfg.WorkerBuffer + 1)
	}
	p := &Pool{
		cfg:       cfg,
		mailboxes: make([]chan job, cfg.MaxWorkers),
		budget:    make(chan struct{}, cfg.QueueBudget),
	}
	for i := range p.mailboxes {
		p.mailboxes[i] = make(chan job, cfg.WorkerBuffer)
		go p.worker(i, p.mailboxes[i])
	}
	return p
}

func (p *Pool) Config() Config { return p.cfg }

func (p *Pool) Submit(ctx context.Context, taskID string, fn func(context.Context) error) error {
	atomic.AddInt64(&p.submitted, 1)
	if err := p.acquireBudget(ctx, taskID); err != nil {
		return err
	}
	atomic.AddInt64(&p.inFlight, 1)

	j := job{ctx: ctx, id: taskID, fn: fn, done: make(chan error, 1)}
	p.wg.Add(1)
	if err := p.deliver(ctx, j); err != nil {
		p.wg.Done()
		atomic.AddInt64(&p.inFlight, -1)
		<-p.budget
		return err
	}

	select {
	case err := <-j.done:
		return err
	case <-ctx.Done():
		go func() {
			<-j.done
		}()
		return ctx.Err()
	}
}

func (p *Pool) Shutdown(ctx context.Context) error {
	for _, ch := range p.mailboxes {
		close(ch)
	}
	done := make(chan struct{})
	go func() {
		p.wg.Wait()
		close(done)
	}()
	select {
	case <-done:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

func (p *Pool) Stats() Stats {
	p.mu.Lock()
	waits := append([]WaitRecord(nil), p.waits...)
	p.mu.Unlock()
	return Stats{
		MaxConcurrency: atomic.LoadInt32(&p.maxConc),
		Submitted:      atomic.LoadInt64(&p.submitted),
		Completed:      atomic.LoadInt64(&p.completed),
		InFlight:       atomic.LoadInt64(&p.inFlight),
		WaitCount:      len(waits),
		Waits:          waits,
	}
}

func (p *Pool) acquireBudget(ctx context.Context, taskID string) error {
	select {
	case p.budget <- struct{}{}:
		return nil
	default:
	}
	start := time.Now()
	select {
	case p.budget <- struct{}{}:
		p.recordWait(taskID, "queue_budget_exhausted", time.Since(start))
		return nil
	case <-ctx.Done():
		p.recordWait(taskID, "canceled_while_waiting_budget", time.Since(start))
		return ctx.Err()
	}
}

func (p *Pool) deliver(ctx context.Context, j job) error {
	startWorker := int(atomic.AddUint64(&p.rr, 1)-1) % len(p.mailboxes)
	for offset := 0; offset < len(p.mailboxes); offset++ {
		idx := (startWorker + offset) % len(p.mailboxes)
		select {
		case p.mailboxes[idx] <- j:
			return nil
		default:
		}
	}
	start := time.Now()
	idx := startWorker
	select {
	case p.mailboxes[idx] <- j:
		p.recordWait(j.id, "worker_buffer_full", time.Since(start))
		return nil
	case <-ctx.Done():
		p.recordWait(j.id, "canceled_while_waiting_worker", time.Since(start))
		return ctx.Err()
	}
}

func (p *Pool) worker(id int, ch <-chan job) {
	for j := range ch {
		func() {
			defer p.wg.Done()
			cur := atomic.AddInt32(&p.concurrency, 1)
			for {
				max := atomic.LoadInt32(&p.maxConc)
				if cur <= max || atomic.CompareAndSwapInt32(&p.maxConc, max, cur) {
					break
				}
			}
			err := safeRun(j.ctx, j.fn)
			atomic.AddInt32(&p.concurrency, -1)
			atomic.AddInt64(&p.completed, 1)
			atomic.AddInt64(&p.inFlight, -1)
			<-p.budget
			j.done <- err
		}()
	}
}

func safeRun(ctx context.Context, fn func(context.Context) error) (err error) {
	defer func() {
		if r := recover(); r != nil {
			err = fmt.Errorf("scheduler task panic: %v", r)
		}
	}()
	return fn(ctx)
}

func (p *Pool) recordWait(taskID, reason string, d time.Duration) {
	if d <= 0 {
		d = time.Millisecond
	}
	rec := WaitRecord{TaskID: taskID, Reason: reason, WaitedMS: d.Milliseconds(), AtUnixMS: time.Now().UnixMilli()}
	if rec.WaitedMS == 0 {
		rec.WaitedMS = 1
	}
	p.mu.Lock()
	p.waits = append(p.waits, rec)
	if len(p.waits) > 200 {
		p.waits = p.waits[len(p.waits)-200:]
	}
	p.mu.Unlock()
}

var ErrBudget = errors.New("queue budget exhausted")
