package gateway

import (
	"sort"
	"sync"
	"time"
)

type BoundedQueue[T any] struct {
	mu    sync.Mutex
	items []T
	cap   int
}

func NewBoundedQueue[T any](capacity int) (*BoundedQueue[T], error) {
	if capacity <= 0 {
		return nil, NewError(CodeBudgetBufferInvalid, "budget.service_buffer", "buffer must be positive").WithContext("value", capacity)
	}
	return &BoundedQueue[T]{items: make([]T, 0, minInt(capacity, 16)), cap: capacity}, nil
}

func (q *BoundedQueue[T]) TryEnqueue(item T) error {
	q.mu.Lock()
	defer q.mu.Unlock()
	if len(q.items) >= q.cap {
		return NewError(CodeBackpressure, "budget.service_buffer", "per-service pending buffer is full").
			WithContext("capacity", q.cap).WithContext("pending", len(q.items))
	}
	q.items = append(q.items, item)
	return nil
}

func (q *BoundedQueue[T]) Drain() []T {
	q.mu.Lock()
	defer q.mu.Unlock()
	out := append([]T(nil), q.items...)
	q.items = q.items[:0]
	return out
}

func (q *BoundedQueue[T]) Len() int {
	q.mu.Lock()
	defer q.mu.Unlock()
	return len(q.items)
}

func (q *BoundedQueue[T]) Cap() int { return q.cap }

type WorkerPool struct {
	workers int
	sem     chan struct{}
}

func NewWorkerPool(budget Budget) (*WorkerPool, error) {
	if err := budget.ValidateBudget(); err != nil {
		return nil, err
	}
	return &WorkerPool{workers: budget.Workers, sem: make(chan struct{}, budget.Workers)}, nil
}

func (p *WorkerPool) Workers() int { return p.workers }

func (p *WorkerPool) TryAcquire() (func(), error) {
	select {
	case p.sem <- struct{}{}:
		return func() { <-p.sem }, nil
	default:
		return nil, NewError(CodeBackpressure, "budget.workers", "worker budget exhausted").
			WithContext("workers", p.workers).WithContext("active", len(p.sem))
	}
}

type ServiceDiagnostic struct {
	ServiceID string  `json:"service_id"`
	Records   int     `json:"records"`
	ElapsedNS int64   `json:"elapsed_ns"`
	ElapsedMS float64 `json:"elapsed_ms"`
}

type ParallelDiagnostics struct {
	Workers       int                 `json:"workers"`
	ServiceBuffer int                 `json:"service_buffer"`
	Services      []ServiceDiagnostic `json:"services"`
	Note          string              `json:"note"`
}

func RunParallel(p *Project) (*RunReport, error) {
	if err := p.Validate(); err != nil {
		return nil, err
	}
	engine, err := NewEngine(p)
	if err != nil {
		return nil, err
	}
	pool, err := NewWorkerPool(p.Budget)
	if err != nil {
		return nil, err
	}

	recordsByService := map[string][]Record{}
	queues := map[string]*BoundedQueue[Record]{}
	for _, service := range p.Services {
		q, err := NewBoundedQueue[Record](p.Budget.ServiceBuffer)
		if err != nil {
			return nil, err
		}
		queues[service.ID] = q
		recordsByService[service.ID] = []Record{}
	}
	for _, record := range p.Records {
		serviceID := record.ServiceID
		if serviceID == "" {
			route, err := p.MatchRoute(record.Path)
			if err != nil {
				return nil, err
			}
			serviceID = route.ServiceID
		}
		q := queues[serviceID]
		if q == nil {
			return nil, NewError(CodeFieldRequired, "record.service_id", "unknown service").WithContext("service_id", serviceID)
		}
		if err := q.TryEnqueue(record); err != nil {
			ge, _ := AsError(err)
			if ge != nil {
				ge.WithContext("service_id", serviceID).WithContext("record_id", record.ID)
			}
			return nil, err
		}
		recordsByService[serviceID] = append(recordsByService[serviceID], record)
	}

	serviceIDs := make([]string, 0, len(p.Services))
	for _, service := range p.Services {
		serviceIDs = append(serviceIDs, service.ID)
	}
	sort.Strings(serviceIDs)

	tasks := make(chan string, len(serviceIDs))
	type partitionOutput struct {
		part *partitionResult
		err  error
	}
	results := make(chan partitionOutput, len(serviceIDs))
	diagnostics := make(chan ServiceDiagnostic, len(serviceIDs))
	var wg sync.WaitGroup
	for i := 0; i < pool.workers; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for serviceID := range tasks {
				release, err := pool.TryAcquire()
				if err != nil {
					results <- partitionOutput{err: err}
					continue
				}
				start := time.Now()
				records := queues[serviceID].Drain()
				part, procErr := engine.processRecords(records, serviceID)
				elapsed := time.Since(start)
				release()
				if procErr != nil {
					results <- partitionOutput{err: procErr}
					continue
				}
				results <- partitionOutput{part: part}
				diagnostics <- ServiceDiagnostic{
					ServiceID: serviceID, Records: len(records), ElapsedNS: elapsed.Nanoseconds(),
					ElapsedMS: float64(elapsed) / float64(time.Millisecond),
				}
			}
		}()
	}
	for _, serviceID := range serviceIDs {
		tasks <- serviceID
	}
	close(tasks)

	go func() {
		wg.Wait()
		close(results)
		close(diagnostics)
	}()

	parts := make([]*partitionResult, 0, len(serviceIDs))
	var firstErr error
	for output := range results {
		if output.err != nil && firstErr == nil {
			firstErr = output.err
		}
		if output.part != nil {
			parts = append(parts, output.part)
		}
	}
	diags := []ServiceDiagnostic{}
	for diag := range diagnostics {
		diags = append(diags, diag)
	}
	sort.Slice(diags, func(i, j int) bool { return diags[i].ServiceID < diags[j].ServiceID })

	if firstErr != nil {
		return nil, firstErr
	}
	report, err := engine.reportFromPartitions(parts)
	if err != nil {
		return nil, err
	}
	report.Diagnostics = &ParallelDiagnostics{
		Workers: p.Budget.Workers, ServiceBuffer: p.Budget.ServiceBuffer, Services: diags,
		Note: "elapsed values are diagnostic only and are excluded from correctness comparison",
	}
	return report, nil
}

func minInt(a, b int) int {
	if a < b {
		return a
	}
	return b
}
