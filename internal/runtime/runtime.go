package runtime

import (
	"sort"
	"sync"
	"sync/atomic"
	"time"

	"tlscheck/internal/bizerr"
	"tlscheck/internal/engine"
	"tlscheck/internal/model"
)

type Config struct {
	Workers        int
	EndpointOrder  []string
	EndpointDelay  time.Duration
	Faults         engine.FaultOptions
	InitialStates  map[string]*engine.EndpointState
	InitialResults map[string][]*engine.RequestResult
}

func RunParallel(project model.Project, cfg Config) (*engine.Report, error) {
	workers := cfg.Workers
	if workers <= 0 {
		workers = project.ThreadBudget
	}
	if workers <= 0 {
		workers = 1
	}
	order := append([]string(nil), cfg.EndpointOrder...)
	if len(order) == 0 {
		for _, ep := range project.Endpoints {
			order = append(order, ep.ID)
		}
		sort.Strings(order)
	}

	sem := make(chan struct{}, workers)
	var active int32
	var peak int32
	var throttles int32
	var wg sync.WaitGroup
	var mu sync.Mutex
	runs := make([]engine.EndpointRun, 0, len(order))
	var firstErr error

	for _, epID := range order {
		select {
		case sem <- struct{}{}:
		default:
			atomic.AddInt32(&throttles, 1)
			sem <- struct{}{}
		}
		wg.Add(1)
		go func(id string) {
			cur := atomic.AddInt32(&active, 1)
			for {
				old := atomic.LoadInt32(&peak)
				if cur <= old || atomic.CompareAndSwapInt32(&peak, old, cur) {
					break
				}
			}
			defer func() {
				atomic.AddInt32(&active, -1)
				<-sem
				wg.Done()
			}()
			if cfg.EndpointDelay > 0 {
				time.Sleep(cfg.EndpointDelay)
			}
			start := time.Now()
			opts := engine.Options{
				Faults: cfg.Faults, InitialStates: cfg.InitialStates,
				InitialResults: cfg.InitialResults, Workers: workers,
			}
			results, state, moments, err := engine.RunEndpoint(project, id, cfg.InitialStates[id], opts)
			elapsed := time.Since(start).Nanoseconds()
			mu.Lock()
			runs = append(runs, engine.EndpointRun{
				ID: id, Results: results, State: state, Moments: moments, Err: err, ElapsedNS: elapsed,
			})
			if err != nil && firstErr == nil {
				firstErr = err
			}
			mu.Unlock()
		}(epID)
	}
	wg.Wait()
	report := engine.MergeRuns(project, runs)
	report.Diagnostics.PeakWorkers = int(atomic.LoadInt32(&peak))
	report.Diagnostics.ThreadThrottles = int(atomic.LoadInt32(&throttles))
	for _, res := range report.Results {
		if res != nil && res.BackpressureCode != "" {
			report.Diagnostics.BackpressureCount++
		}
	}
	if firstErr != nil {
		return report, firstErr
	}
	return report, nil
}

type Lease struct {
	pool *BoundedPool
}

func (l *Lease) Release() {
	if l != nil && l.pool != nil {
		l.pool.release()
	}
}

type BoundedPool struct {
	sem    chan struct{}
	active int32
	peak   int32
}

func NewBoundedPool(workers int) (*BoundedPool, error) {
	if workers <= 0 {
		return nil, bizerr.New(bizerr.EThreadBudget, "thread budget must be positive").
			WithContext("workers", workers)
	}
	return &BoundedPool{sem: make(chan struct{}, workers)}, nil
}

func (p *BoundedPool) TryAcquire() (*Lease, error) {
	select {
	case p.sem <- struct{}{}:
		cur := atomic.AddInt32(&p.active, 1)
		for {
			old := atomic.LoadInt32(&p.peak)
			if cur <= old || atomic.CompareAndSwapInt32(&p.peak, old, cur) {
				break
			}
		}
		return &Lease{pool: p}, nil
	default:
		return nil, bizerr.Newf(bizerr.EThreadBudget, "thread budget %d exhausted", cap(p.sem)).
			WithContext("thread_budget", cap(p.sem))
	}
}

func (p *BoundedPool) release() {
	atomic.AddInt32(&p.active, -1)
	<-p.sem
}

func (p *BoundedPool) Peak() int { return int(atomic.LoadInt32(&p.peak)) }

type Mailbox struct {
	EndpointID string
	ch         chan *model.Request
}

func NewMailbox(endpointID string, capacity int) (*Mailbox, error) {
	if capacity <= 0 {
		return nil, bizerr.New(bizerr.EBufferFull, "buffer capacity must be positive").
			WithContext("endpoint_id", endpointID)
	}
	return &Mailbox{EndpointID: endpointID, ch: make(chan *model.Request, capacity)}, nil
}

func (m *Mailbox) TrySend(req *model.Request) error {
	select {
	case m.ch <- req:
		return nil
	default:
		return bizerr.Newf(bizerr.EBufferFull, "endpoint %q pending buffer %d is full", m.EndpointID, cap(m.ch)).
			WithContext("endpoint_id", m.EndpointID).WithContext("buffer_capacity", cap(m.ch))
	}
}

func (m *Mailbox) Chan() <-chan *model.Request { return m.ch }
