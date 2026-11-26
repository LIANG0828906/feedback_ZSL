// Package runtime provides bounded parallel execution and diagnostics.
// Timing observations are diagnostic only and never participate in equality
// or admission decisions.
package runtime

import (
	"sync"
	"time"

	"quotakernel/gate"
	"quotakernel/model"
)

type Observation struct {
	Count   int   `json:"count"`
	TotalNs int64 `json:"totalNs"`
	LastNs  int64 `json:"lastNs"`
	MaxNs   int64 `json:"maxNs"`
}

type MetricsSnapshot struct {
	Tenants       map[string]Observation `json:"tenants"`
	Backpressure  map[string]int         `json:"backpressure"`
	AcceptedTasks int64                  `json:"acceptedTasks"`
	RejectedTasks int64                  `json:"rejectedTasks"`
}

type Metrics struct {
	mu            sync.Mutex
	tenants       map[string]*Observation
	backpressure  map[string]int
	acceptedTasks int64
	rejectedTasks int64
}

func NewMetrics() *Metrics {
	return &Metrics{tenants: map[string]*Observation{}, backpressure: map[string]int{}}
}

func (m *Metrics) observe(tenant string, elapsed time.Duration) {
	m.mu.Lock()
	defer m.mu.Unlock()
	obs := m.tenants[tenant]
	if obs == nil {
		obs = &Observation{}
		m.tenants[tenant] = obs
	}
	ns := elapsed.Nanoseconds()
	obs.Count++
	obs.TotalNs += ns
	obs.LastNs = ns
	if ns > obs.MaxNs {
		obs.MaxNs = ns
	}
	m.acceptedTasks++
}

func (m *Metrics) reject(code string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.backpressure[code]++
	m.rejectedTasks++
}

func (m *Metrics) Snapshot() MetricsSnapshot {
	m.mu.Lock()
	defer m.mu.Unlock()
	out := MetricsSnapshot{
		Tenants:       map[string]Observation{},
		Backpressure:  map[string]int{},
		AcceptedTasks: m.acceptedTasks,
		RejectedTasks: m.rejectedTasks,
	}
	for k, v := range m.tenants {
		out.Tenants[k] = *v
	}
	for k, v := range m.backpressure {
		out.Backpressure[k] = v
	}
	return out
}

type task struct {
	tenant string
	fn     func() error
}

// Pool is a fixed-worker bounded-queue scheduler. Submit never blocks on
// exhaustion; it returns the stable BP_WORKER_BUDGET_EXHAUSTED code.
type Pool struct {
	workers   int
	queue     chan task
	wg        sync.WaitGroup
	Metrics   *Metrics
	closeOnce sync.Once
}

func NewPool(workers, queueSize int) *Pool {
	if workers <= 0 {
		workers = model.DefaultRules().WorkerBudget
	}
	if queueSize < 0 {
		queueSize = 0
	}
	p := &Pool{workers: workers, queue: make(chan task, queueSize), Metrics: NewMetrics()}
	for i := 0; i < workers; i++ {
		go p.worker()
	}
	return p
}

func (p *Pool) worker() {
	for item := range p.queue {
		start := time.Now()
		_ = item.fn()
		p.Metrics.observe(item.tenant, time.Since(start))
		p.wg.Done()
	}
}

// Submit returns model.CodeAdmitted when queued, or a backpressure code.
func (p *Pool) Submit(tenant string, fn func() error) string {
	p.wg.Add(1)
	select {
	case p.queue <- task{tenant: tenant, fn: fn}:
		return model.CodeAdmitted
	default:
		p.wg.Done()
		p.Metrics.reject(model.CodeBackpressureWorkers)
		return model.CodeBackpressureWorkers
	}
}

func (p *Pool) Wait() {
	p.wg.Wait()
}

func (p *Pool) CloseAndWait() {
	p.closeOnce.Do(func() { close(p.queue) })
	p.wg.Wait()
}

// RunGateBatch evaluates independent gate decisions using the bounded pool.
// Result indices match input indices, making output order deterministic even
// though worker scheduling is not.
func RunGateBatch(g *gate.FastGate, inputs []gate.Input, workers int) ([]gate.Decision, MetricsSnapshot) {
	pool := NewPool(workers, len(inputs))
	decisions := make([]gate.Decision, len(inputs))
	for i := range inputs {
		i := i
		code := pool.Submit(inputs[i].TenantID, func() error {
			decisions[i] = g.Decide(inputs[i])
			return nil
		})
		if code != model.CodeAdmitted {
			decisions[i] = gate.Decision{Record: model.GateRecord{
				EventID: inputs[i].EventID, Conclusion: gate.ConclusionQueue,
				ReasonCode: code, ConstraintID: model.ConstraintQueueBound,
			}}
		}
	}
	pool.CloseAndWait()
	return decisions, pool.Metrics.Snapshot()
}

// EventMailbox collects concurrently submitted events and drains them as one
// deterministic tick batch before canonical settlement.
type EventMailbox struct {
	mu     sync.Mutex
	events []model.Event
}

func (m *EventMailbox) Submit(events ...model.Event) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.events = append(m.events, events...)
}

func (m *EventMailbox) Drain() []model.Event {
	m.mu.Lock()
	defer m.mu.Unlock()
	out := m.events
	m.events = nil
	return out
}
