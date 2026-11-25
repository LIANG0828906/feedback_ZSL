package hckernel

import (
	"context"
	"sort"
	"sync"
	"time"
)

type ProbeOutput struct {
	ProbeID    string          `json:"probe_id"`
	InstanceID string          `json:"instance_id"`
	WorkerID   int             `json:"worker_id"`
	Result     ProbeResultKind `json:"result"`
	Message    string          `json:"message,omitempty"`
}

type BatchResult struct {
	Round       int           `json:"round"`
	ProbeCount  int           `json:"probe_count"`
	WorkersUsed int           `json:"workers_used"`
	Duration    time.Duration `json:"duration_ns"`
	Outputs     []ProbeOutput `json:"outputs"`
}

type ParallelExecutor struct {
	workers int
	buffer  int
	sem     chan struct{}
}

func NewParallelExecutor(workers, bufferCapacity int) (*ParallelExecutor, error) {
	if workers <= 0 {
		return nil, newError(ErrWorkerBudgetInvalid, "worker budget must be positive")
	}
	if bufferCapacity < 0 {
		return nil, newError(ErrBackpressureBufferFull, "buffer capacity cannot be negative")
	}
	return &ParallelExecutor{workers: workers, buffer: bufferCapacity, sem: make(chan struct{}, workers)}, nil
}

func (x *ParallelExecutor) WorkerBudget() int   { return x.workers }
func (x *ParallelExecutor) BufferCapacity() int { return x.buffer }

type WorkerLease struct {
	e *ParallelExecutor
}

func (l WorkerLease) Release() {
	if l.e != nil {
		<-l.e.sem
	}
}

// TryReserve atomically reserves n workers without allocating beyond budget.
func (x *ParallelExecutor) TryReserve(n int) ([]WorkerLease, error) {
	if n <= 0 {
		return nil, newError(ErrBackpressureWorker, "reservation count must be positive")
	}
	if n > x.workers {
		return nil, newError(ErrBackpressureWorker, "requested worker parallelism exceeds declared budget")
	}
	leases := make([]WorkerLease, 0, n)
	for i := 0; i < n; i++ {
		select {
		case x.sem <- struct{}{}:
			leases = append(leases, WorkerLease{e: x})
		default:
			for _, lease := range leases {
				lease.Release()
			}
			return nil, newError(ErrBackpressureWorkersBusy, "worker budget exhausted; retry later")
		}
	}
	return leases, nil
}

// ExecuteBatch runs one round's probes with at most parallelism workers. The
// returned outputs are sorted by probe id so completion order cannot affect
// later deterministic reduction.
func (x *ParallelExecutor) ExecuteBatch(
	ctx context.Context,
	round int,
	probes []Probe,
	parallelism int,
	handler func(context.Context, Probe) (ProbeOutput, error),
) (*BatchResult, error) {
	if parallelism <= 0 {
		parallelism = x.workers
	}
	if parallelism > x.workers {
		return nil, newError(ErrBackpressureWorker, "requested parallelism exceeds worker budget")
	}
	if len(probes) > x.buffer {
		return nil, newError(
			ErrBackpressureBufferFull,
			"probe batch exceeds per-round buffer capacity",
			withConstraint("C-PROBE-BUFFER"),
		)
	}
	if handler == nil {
		handler = func(_ context.Context, probe Probe) (ProbeOutput, error) {
			return ProbeOutput{ProbeID: probe.ID, InstanceID: probe.InstanceID, Result: ResultSuccess}, nil
		}
	}

	workers := parallelism
	if workers > len(probes) {
		workers = len(probes)
	}
	if workers == 0 {
		return &BatchResult{Round: round, Outputs: []ProbeOutput{}}, nil
	}

	jobs := make(chan Probe, x.buffer)
	results := make(chan ProbeOutput, len(probes))
	errs := make(chan error, len(probes))
	start := time.Now()

	var wg sync.WaitGroup
	for workerID := 1; workerID <= workers; workerID++ {
		wg.Add(1)
		go func(id int) {
			defer wg.Done()
			for probe := range jobs {
				if ctx.Err() != nil {
					errs <- ctx.Err()
					return
				}
				out, err := handler(ctx, probe)
				if err != nil {
					errs <- err
					continue
				}
				if out.ProbeID == "" {
					out.ProbeID = probe.ID
				}
				if out.InstanceID == "" {
					out.InstanceID = probe.InstanceID
				}
				out.WorkerID = id
				results <- out
			}
		}(workerID)
	}
	for _, probe := range probes {
		select {
		case jobs <- probe:
		case <-ctx.Done():
			close(jobs)
			wg.Wait()
			return nil, ctx.Err()
		}
	}
	close(jobs)
	wg.Wait()
	close(results)
	close(errs)
	duration := time.Since(start)

	for err := range errs {
		if err != nil {
			return nil, err
		}
	}
	outputs := make([]ProbeOutput, 0, len(probes))
	for out := range results {
		outputs = append(outputs, out)
	}
	sort.Slice(outputs, func(i, j int) bool { return outputs[i].ProbeID < outputs[j].ProbeID })
	return &BatchResult{
		Round:       round,
		ProbeCount:  len(probes),
		WorkersUsed: workers,
		Duration:    duration,
		Outputs:     outputs,
	}, nil
}

// ShuffleForTest deterministically reverses and rotates a probe batch to prove
// submission order does not change canonical reduction.
func ShuffleForTest(probes []Probe, seed int) []Probe {
	out := append([]Probe(nil), probes...)
	sort.SliceStable(out, func(i, j int) bool {
		ki := (seed*1103515245 + i*12345) & 0x7fffffff
		kj := (seed*1103515245 + j*12345) & 0x7fffffff
		if ki != kj {
			return ki < kj
		}
		return out[i].ID < out[j].ID
	})
	return out
}
