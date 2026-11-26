package sched

import (
	"context"
	"sort"
	"sync"

	"meshpolicy/internal/xerr"
)

const (
	MinWorkers     = 1
	MaxWorkers     = 64
	MinBuffer      = 1
	MaxBuffer      = 4096
	DefaultWorkers = 4
	DefaultBuffer  = 64
)

type Config struct {
	Workers          int            `json:"workers"`
	BufferPerNS      int            `json:"buffer_per_ns"`
	NamespaceBuffers map[string]int `json:"namespace_buffers,omitempty"`
}

func (c Config) Normalize() (Config, error) {
	if c.Workers == 0 {
		c.Workers = DefaultWorkers
	}
	if c.Workers < MinWorkers || c.Workers > MaxWorkers {
		return c, xerr.New(xerr.EBudgetWorkerInvalid, "worker budget is outside declared bounds", map[string]any{
			"workers": c.Workers, "min": MinWorkers, "max": MaxWorkers,
		})
	}
	if c.BufferPerNS == 0 {
		c.BufferPerNS = DefaultBuffer
	}
	if c.BufferPerNS < MinBuffer || c.BufferPerNS > MaxBuffer {
		return c, xerr.New(xerr.EBudgetBufferInvalid, "namespace buffer budget is outside declared bounds", map[string]any{
			"buffer_per_ns": c.BufferPerNS, "min": MinBuffer, "max": MaxBuffer,
		})
	}
	for ns, n := range c.NamespaceBuffers {
		if n < MinBuffer || n > MaxBuffer {
			return c, xerr.New(xerr.EBudgetBufferInvalid, "namespace buffer override is outside declared bounds", map[string]any{
				"namespace": ns, "buffer": n, "min": MinBuffer, "max": MaxBuffer,
			})
		}
	}
	return c, nil
}

type Pipeline[T any] struct {
	cfg       Config
	workerSem chan struct{}
	mu        sync.Mutex
	queues    map[string]chan T
	started   bool
	wg        sync.WaitGroup
	errMu     sync.Mutex
	err       error
}

func NewPipeline[T any](cfg Config) (*Pipeline[T], error) {
	ncfg, err := cfg.Normalize()
	if err != nil {
		return nil, err
	}
	return &Pipeline[T]{
		cfg:       ncfg,
		workerSem: make(chan struct{}, ncfg.Workers),
		queues:    map[string]chan T{},
	}, nil
}

func (p *Pipeline[T]) Register(namespace string) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if _, ok := p.queues[namespace]; !ok {
		buf := p.cfg.BufferPerNS
		if n, ok := p.cfg.NamespaceBuffers[namespace]; ok {
			buf = n
		}
		p.queues[namespace] = make(chan T, buf)
	}
}

func (p *Pipeline[T]) Namespaces() []string {
	p.mu.Lock()
	defer p.mu.Unlock()
	out := make([]string, 0, len(p.queues))
	for ns := range p.queues {
		out = append(out, ns)
	}
	sort.Strings(out)
	return out
}

func (p *Pipeline[T]) Submit(namespace string, item T) error {
	p.Register(namespace)
	p.mu.Lock()
	ch := p.queues[namespace]
	buf := cap(ch)
	p.mu.Unlock()
	select {
	case ch <- item:
		return nil
	default:
		return xerr.New(xerr.EBackpressure, "namespace pending judgment buffer is full", map[string]any{
			"code": xerr.ENamespaceBufferFull, "namespace": namespace, "buffer_capacity": buf,
		})
	}
}

func (p *Pipeline[T]) Start(ctx context.Context, handle func(namespace string, item T) error) {
	p.mu.Lock()
	if p.started {
		p.mu.Unlock()
		return
	}
	p.started = true
	namespaces := make([]string, 0, len(p.queues))
	channels := map[string]chan T{}
	for ns, ch := range p.queues {
		namespaces = append(namespaces, ns)
		channels[ns] = ch
	}
	p.mu.Unlock()

	for _, ns := range namespaces {
		ch := channels[ns]
		p.wg.Add(1)
		go func(namespace string, queue chan T) {
			defer p.wg.Done()
			for item := range queue {
				select {
				case p.workerSem <- struct{}{}:
				case <-ctx.Done():
					p.setError(ctx.Err())
					return
				}
				err := handle(namespace, item)
				<-p.workerSem
				if err != nil {
					p.setError(err)
					return
				}
			}
		}(ns, ch)
	}
}

func (p *Pipeline[T]) setError(err error) {
	if err == nil {
		return
	}
	p.errMu.Lock()
	if p.err == nil {
		p.err = err
	}
	p.errMu.Unlock()
}

func (p *Pipeline[T]) CloseAndWait() error {
	p.mu.Lock()
	for _, ch := range p.queues {
		close(ch)
	}
	p.mu.Unlock()
	p.wg.Wait()
	p.errMu.Lock()
	defer p.errMu.Unlock()
	return p.err
}

type Admission struct {
	workerSem chan struct{}
	mu        sync.Mutex
	nsTokens  map[string]chan struct{}
	cfg       Config
}

func NewAdmission(cfg Config) (*Admission, error) {
	ncfg, err := cfg.Normalize()
	if err != nil {
		return nil, err
	}
	return &Admission{workerSem: make(chan struct{}, ncfg.Workers), nsTokens: map[string]chan struct{}{}, cfg: ncfg}, nil
}

func (a *Admission) TryAdmit(namespace string) (func(), error) {
	select {
	case a.workerSem <- struct{}{}:
	default:
		return nil, xerr.New(xerr.EWorkerBudgetExhausted, "all declared worker slots are busy", map[string]any{
			"workers": cap(a.workerSem), "namespace": namespace,
		})
	}
	a.mu.Lock()
	ch, ok := a.nsTokens[namespace]
	if !ok {
		buf := a.cfg.BufferPerNS
		if n, ok := a.cfg.NamespaceBuffers[namespace]; ok {
			buf = n
		}
		ch = make(chan struct{}, buf)
		a.nsTokens[namespace] = ch
	}
	a.mu.Unlock()
	select {
	case ch <- struct{}{}:
	default:
		<-a.workerSem
		return nil, xerr.New(xerr.EBackpressure, "namespace pending buffer is full", map[string]any{
			"code": xerr.ENamespaceBufferFull, "namespace": namespace, "buffer_capacity": cap(ch),
		})
	}
	return func() {
		<-ch
		<-a.workerSem
	}, nil
}
