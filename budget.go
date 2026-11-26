package downsample

import (
	"context"
	"sync"
)

const (
	MaxAllowedWorkers = 64
	MaxAllowedBuffer  = 100_000
)

// Budget is the declared parallelism and per-metric window buffer ceiling.
type Budget struct {
	MaxWorkers          int `json:"max_workers"`
	MaxWindowsPerMetric int `json:"max_windows_per_metric"`
}

func DefaultBudget() Budget {
	return Budget{MaxWorkers: 4, MaxWindowsPerMetric: 64}
}

func (b Budget) Validate() *Error {
	if b.MaxWorkers <= 0 || b.MaxWorkers > MaxAllowedWorkers {
		return NewError(CodeBudgetWorkersOutOfRange, "worker budget is out of declared range [1,64]").
			WithFieldPath("budget.max_workers").
			WithContext("value", b.MaxWorkers).
			WithContext("min", 1).WithContext("max", MaxAllowedWorkers)
	}
	if b.MaxWindowsPerMetric <= 0 || b.MaxWindowsPerMetric > MaxAllowedBuffer {
		return NewError(CodeBudgetBufferOutOfRange, "window buffer budget is out of declared range [1,100000]").
			WithFieldPath("budget.max_windows_per_metric").
			WithContext("value", b.MaxWindowsPerMetric).
			WithContext("min", 1).WithContext("max", MaxAllowedBuffer)
	}
	return nil
}

// Gate enforces both budgets. TryAcquire is intentionally non-blocking so an
// exhausted budget produces an observable stable E_BACKPRESSURE_* code instead
// of an unbounded queue or dropped window.
type Gate struct {
	budget  Budget
	workers chan struct{}
	mu      sync.Mutex
	buffers map[string]chan struct{}
}

func NewGate(budget Budget) (*Gate, *Error) {
	if err := budget.Validate(); err != nil {
		return nil, err
	}
	return &Gate{
		budget:  budget,
		workers: make(chan struct{}, budget.MaxWorkers),
		buffers: map[string]chan struct{}{},
	}, nil
}

func (g *Gate) metricBuffer(metricID string) chan struct{} {
	g.mu.Lock()
	defer g.mu.Unlock()
	ch, ok := g.buffers[metricID]
	if !ok {
		ch = make(chan struct{}, g.budget.MaxWindowsPerMetric)
		g.buffers[metricID] = ch
	}
	return ch
}

func (g *Gate) TryAcquire(metricID string) (func(), *Error) {
	select {
	case g.workers <- struct{}{}:
	default:
		return nil, NewError(CodeBackpressureWorkers, "worker budget exhausted; apply backpressure before submitting more metrics/windows").
			WithContext("metric_id", metricID).
			WithContext("max_workers", g.budget.MaxWorkers)
	}

	buffer := g.metricBuffer(metricID)
	select {
	case buffer <- struct{}{}:
		var once sync.Once
		return func() {
			once.Do(func() {
				<-buffer
				<-g.workers
			})
		}, nil
	default:
		<-g.workers
		return nil, NewError(CodeBackpressureBuffer, "per-metric window buffer exhausted; no window allocation was performed").
			WithContext("metric_id", metricID).
			WithContext("max_windows_per_metric", g.budget.MaxWindowsPerMetric)
	}
}

func (g *Gate) Acquire(ctx context.Context, metricID string) (func(), error) {
	select {
	case g.workers <- struct{}{}:
	case <-ctx.Done():
		return nil, ctx.Err()
	}
	buffer := g.metricBuffer(metricID)
	select {
	case buffer <- struct{}{}:
	case <-ctx.Done():
		<-g.workers
		return nil, ctx.Err()
	}
	var once sync.Once
	return func() {
		once.Do(func() {
			<-buffer
			<-g.workers
		})
	}, nil
}
