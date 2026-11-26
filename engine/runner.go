package engine

import (
	"context"
	"sync"
	"time"

	"dnslab/codes"
	"dnslab/fixedint"
	"dnslab/spec"
)

// Budget is a non-blocking observable worker-slot budget.
type Budget struct {
	budget int
	sem    chan struct{}
}

func NewBudget(workers int) (*Budget, error) {
	if err := fixedint.ValidateWorkerBudget(workers); err != nil {
		return nil, err
	}
	return &Budget{budget: workers, sem: make(chan struct{}, workers)}, nil
}

func (b *Budget) Max() int { return b.budget }

func (b *Budget) InFlight() int { return len(b.sem) }

// TryAcquire returns E_BACKPRESSURE_WORKER_BUDGET immediately when exhausted.
func (b *Budget) TryAcquire() error {
	select {
	case b.sem <- struct{}{}:
		return nil
	default:
		return codes.New(codes.EBackpressureWorkers, "declared worker budget is exhausted").
			WithContext("worker_budget", b.budget).
			WithContext("in_flight", len(b.sem)).
			WithContext("retryable", true)
	}
}

func (b *Budget) Release() {
	select {
	case <-b.sem:
	default:
	}
}

// TryProcessQuery acquires a budget slot before processing one query.
func TryProcessQuery(e *Engine, b *Budget, q spec.Query) (Result, error) {
	if err := b.TryAcquire(); err != nil {
		return Result{Sequence: q.Sequence, QueryID: q.ID, Code: codes.EBackpressureWorkers}, err
	}
	defer b.Release()
	return e.ProcessQuery(q), nil
}

// Report contains ordered results and actual wall-time observation.
type Report struct {
	Results          []Result `json:"results"`
	WorkerBudget     int      `json:"worker_budget"`
	QueryCount       int      `json:"query_count"`
	WallElapsedNS    int64    `json:"wall_elapsed_ns"`
	WallElapsedMS    float64  `json:"wall_elapsed_ms"`
	LogicalOrderKept bool     `json:"logical_order_kept"`
}

// RunSamples processes all samples with a fixed worker pool. Results are
// returned in sample sequence order regardless of completion order.
func RunSamples(ctx context.Context, e *Engine, queries []spec.Query, workers int) (*Report, error) {
	if err := fixedint.ValidateWorkerBudget(workers); err != nil {
		return nil, err
	}
	if ctx == nil {
		ctx = context.Background()
	}
	if err := ctx.Err(); err != nil {
		return nil, codes.Wrap(codes.EInterrupted, "run canceled before start", err)
	}

	started := time.Now()
	results := make([]Result, len(queries))
	jobs := make(chan int)
	var wg sync.WaitGroup

	for w := 0; w < workers; w++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for idx := range jobs {
				if ctx.Err() != nil {
					results[idx] = Result{
						Sequence: queries[idx].Sequence, QueryID: queries[idx].ID,
						Code: codes.EInterrupted, Source: spec.SourceError,
						ErrorContext: map[string]any{"cause": ctx.Err().Error()},
					}
					continue
				}
				results[idx] = e.ProcessQuery(queries[idx])
			}
		}()
	}

	for i := range queries {
		select {
		case <-ctx.Done():
			close(jobs)
			wg.Wait()
			return nil, codes.Wrap(codes.EInterrupted, "run canceled during dispatch", ctx.Err())
		case jobs <- i:
		}
	}
	close(jobs)
	wg.Wait()

	elapsed := time.Since(started)
	return &Report{
		Results: results, WorkerBudget: workers, QueryCount: len(queries),
		WallElapsedNS: elapsed.Nanoseconds(), WallElapsedMS: float64(elapsed.Microseconds()) / 1000.0,
		LogicalOrderKept: true,
	}, nil
}

// CompareSemantic checks the fields that must remain schedule-independent.
func CompareSemantic(got Result, want spec.Truth) bool {
	if got.Code != want.Code || got.Matched != want.Matched {
		return false
	}
	if got.RuleNumber != want.RuleNumber || got.UpstreamIndex != want.UpstreamIndex {
		return false
	}
	if got.HopCount != want.HopCount || got.Source != want.Source || got.Negative != want.Negative {
		return false
	}
	if want.DecisionBasis != "" && got.DecisionBasis != want.DecisionBasis {
		return false
	}
	if len(got.CandidateRules) != len(want.CandidateRules) {
		return false
	}
	for i := range want.CandidateRules {
		if got.CandidateRules[i] != want.CandidateRules[i] {
			return false
		}
	}
	if len(got.Records) != len(want.Records) || len(got.RemainingTTLMS) != len(want.RemainingTTLMS) {
		return false
	}
	for i := range want.Records {
		if got.Records[i].Name != want.Records[i].Name ||
			got.Records[i].Type != want.Records[i].Type ||
			got.Records[i].Value != want.Records[i].Value {
			return false
		}
	}
	for i := range want.RemainingTTLMS {
		if got.RemainingTTLMS[i] != want.RemainingTTLMS[i] {
			return false
		}
	}
	if want.EntryRemainingMS != 0 && got.EntryRemainingMS != want.EntryRemainingMS {
		return false
	}
	if want.VirtualElapsedMS != 0 && got.VirtualElapsedMS != want.VirtualElapsedMS {
		return false
	}
	return true
}
