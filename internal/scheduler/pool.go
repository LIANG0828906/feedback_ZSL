package scheduler

import (
	"context"
	"sort"
	"sync"
	"sync/atomic"
	"time"

	"presentkernel/internal/domain"
)

type Config struct {
	MaxParallel     int `json:"max_parallel"`
	PerWorkerBuffer int `json:"per_worker_buffer"`
	QueueBudget     int `json:"queue_budget"`
}

func (c Config) Normalize() Config {
	if c.MaxParallel <= 0 {
		c.MaxParallel = 2
	}
	if c.PerWorkerBuffer <= 0 {
		c.PerWorkerBuffer = 1
	}
	if c.QueueBudget <= 0 {
		c.QueueBudget = c.MaxParallel * c.PerWorkerBuffer
	}
	if c.QueueBudget < c.MaxParallel {
		c.QueueBudget = c.MaxParallel
	}
	return c
}

type Job struct {
	Index int
	Ref   domain.PartitionRef
	Run   func(ctx context.Context) (*domain.PartitionResult, error)
}

type Outcome struct {
	Index      int                     `json:"index"`
	Ref        domain.PartitionRef     `json:"ref"`
	Result     *domain.PartitionResult `json:"result,omitempty"`
	Error      *domain.Error           `json:"error,omitempty"`
	DurationMS int64                   `json:"duration_ms"`
}

type QueueEvent struct {
	PartitionID string `json:"partition_id"`
	DialogueID  string `json:"dialogue_id"`
	Reason      string `json:"reason"`
	WaitedMS    int64  `json:"waited_ms"`
}

type Report struct {
	Config              Config       `json:"config"`
	Outcomes            []*Outcome   `json:"outcomes"`
	QueueEvents         []QueueEvent `json:"queue_events"`
	MaxObservedParallel int32        `json:"max_observed_parallel"`
	QueueBlocked        bool         `json:"queue_blocked"`
	BufferBlocked       bool         `json:"buffer_blocked"`
}

type worker struct {
	jobs chan Job
}

type Pool struct {
	cfg        Config
	workers    []*worker
	queueSlots chan struct{}
	outcomes   chan *Outcome
	wg         sync.WaitGroup
	eventsMu   sync.Mutex
	events     []QueueEvent
	active     int32
	maxActive  int32
	rr         uint64
	ctx        context.Context
	cancel     context.CancelFunc
}

func NewPool(parent context.Context, cfg Config) *Pool {
	cfg = cfg.Normalize()
	ctx, cancel := context.WithCancel(parent)
	p := &Pool{
		cfg: cfg, queueSlots: make(chan struct{}, cfg.QueueBudget),
		outcomes: make(chan *Outcome, cfg.QueueBudget+1), ctx: ctx, cancel: cancel,
	}
	for i := 0; i < cfg.MaxParallel; i++ {
		w := &worker{jobs: make(chan Job, cfg.PerWorkerBuffer)}
		p.workers = append(p.workers, w)
		p.wg.Add(1)
		go p.loop(w)
	}
	go func() {
		p.wg.Wait()
		close(p.outcomes)
	}()
	return p
}

func (p *Pool) loop(w *worker) {
	defer p.wg.Done()
	for job := range w.jobs {
		cur := atomic.AddInt32(&p.active, 1)
		for {
			max := atomic.LoadInt32(&p.maxActive)
			if cur <= max || atomic.CompareAndSwapInt32(&p.maxActive, max, cur) {
				break
			}
		}
		start := time.Now()
		out := &Outcome{Index: job.Index, Ref: job.Ref}
		func() {
			defer func() {
				if rec := recover(); rec != nil {
					out.Error = domain.StageError("scheduler recovered panic: %v", rec)
					out.Error.DialogueID = job.Ref.DialogueID
					out.Error.PartitionID = job.Ref.PartitionID
					out.Error.PartitionIndex = job.Ref.PartitionIndex
				}
			}()
			r, err := job.Run(p.ctx)
			if err != nil {
				if de, ok := domain.AsError(err); ok {
					out.Error = de
				} else {
					out.Error = domain.StageError("%s", err.Error())
				}
			} else {
				out.Result = r
			}
		}()
		out.DurationMS = time.Since(start).Milliseconds()
		p.outcomes <- out
		atomic.AddInt32(&p.active, -1)
		<-p.queueSlots
	}
}

func (p *Pool) record(e QueueEvent) {
	p.eventsMu.Lock()
	p.events = append(p.events, e)
	p.eventsMu.Unlock()
}

func (p *Pool) Submit(job Job) error {
	if len(p.queueSlots) >= cap(p.queueSlots) {
		start := time.Now()
		select {
		case p.queueSlots <- struct{}{}:
		case <-p.ctx.Done():
			return p.ctx.Err()
		}
		p.record(QueueEvent{PartitionID: job.Ref.PartitionID, DialogueID: job.Ref.DialogueID, Reason: "queue_budget_exhausted", WaitedMS: time.Since(start).Milliseconds()})
	} else {
		p.queueSlots <- struct{}{}
	}

	n := len(p.workers)
	start := int(atomic.AddUint64(&p.rr, 1)-1) % n
	wi := start
	bestLen := len(p.workers[wi].jobs)
	for offset := 1; offset < n; offset++ {
		i := (start + offset) % n
		if l := len(p.workers[i].jobs); l < bestLen {
			bestLen = l
			wi = i
		}
	}
	w := p.workers[wi]
	if len(w.jobs) >= cap(w.jobs) {
		start := time.Now()
		select {
		case w.jobs <- job:
		case <-p.ctx.Done():
			<-p.queueSlots
			return p.ctx.Err()
		}
		p.record(QueueEvent{PartitionID: job.Ref.PartitionID, DialogueID: job.Ref.DialogueID, Reason: "per_worker_buffer_exhausted", WaitedMS: time.Since(start).Milliseconds()})
		return nil
	}
	select {
	case w.jobs <- job:
	case <-p.ctx.Done():
		<-p.queueSlots
		return p.ctx.Err()
	}
	return nil
}

func (p *Pool) Close() {
	for _, w := range p.workers {
		close(w.jobs)
	}
}

func (p *Pool) Outcomes() <-chan *Outcome { return p.outcomes }

func RunJobs(ctx context.Context, jobs []Job, cfg Config) *Report {
	pool := NewPool(ctx, cfg)
	var outcomesMu sync.Mutex
	outcomes := []*Outcome{}
	collected := make(chan struct{})
	go func() {
		for out := range pool.Outcomes() {
			outcomesMu.Lock()
			outcomes = append(outcomes, out)
			outcomesMu.Unlock()
		}
		close(collected)
	}()
	for _, job := range jobs {
		if err := pool.Submit(job); err != nil {
			de := domain.StageError("scheduler submit canceled: %v", err)
			ref := job.Ref
			de.DialogueID, de.PartitionID, de.PartitionIndex = ref.DialogueID, ref.PartitionID, ref.PartitionIndex
			pool.outcomes <- &Outcome{Index: job.Index, Ref: job.Ref, Error: de}
		}
	}
	pool.Close()
	<-collected
	sort.Slice(outcomes, func(i, j int) bool { return outcomes[i].Index < outcomes[j].Index })
	events := append([]QueueEvent(nil), pool.events...)
	report := &Report{Config: cfg.Normalize(), Outcomes: outcomes, QueueEvents: events, MaxObservedParallel: atomic.LoadInt32(&pool.maxActive)}
	for _, e := range events {
		if e.Reason == "queue_budget_exhausted" {
			report.QueueBlocked = true
		}
		if e.Reason == "per_worker_buffer_exhausted" {
			report.BufferBlocked = true
		}
	}
	return report
}

func (r *Report) SuccessfulResults() []*domain.PartitionResult {
	out := []*domain.PartitionResult{}
	for _, o := range r.Outcomes {
		if o.Error == nil && o.Result != nil {
			out = append(out, o.Result)
		}
	}
	return out
}

func (r *Report) FirstError() error {
	for _, o := range r.Outcomes {
		if o.Error != nil {
			return o.Error
		}
	}
	return nil
}
