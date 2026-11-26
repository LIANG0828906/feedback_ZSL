package queryopt

import "sync"

type BoundedScheduler struct {
	workers int
	buffer  int
	sem     chan struct{}
}

func NewBoundedScheduler(workers, candidateBuffer int) (*BoundedScheduler, error) {
	if err := ValidateBudget(workers, candidateBuffer); err != nil {
		return nil, err
	}
	return &BoundedScheduler{workers: workers, buffer: candidateBuffer, sem: make(chan struct{}, workers)}, nil
}

func (s *BoundedScheduler) WorkerBudget() int    { return s.workers }
func (s *BoundedScheduler) CandidateBuffer() int { return s.buffer }

func (s *BoundedScheduler) TryAcquire() bool {
	select {
	case s.sem <- struct{}{}:
		return true
	default:
		return false
	}
}

func (s *BoundedScheduler) Acquire() { s.sem <- struct{}{} }
func (s *BoundedScheduler) Release() { <-s.sem }

type ScheduledQuery struct {
	Ordinal      int        `json:"ordinal"`
	QueryID      string     `json:"query_id"`
	Admitted     bool       `json:"admitted"`
	Plan         *QueryPlan `json:"plan,omitempty"`
	ErrorCode    string     `json:"error_code,omitempty"`
	ErrorMessage string     `json:"error_message,omitempty"`
}

type ParallelReport struct {
	WorkerBudget    int              `json:"worker_budget"`
	CandidateBuffer int              `json:"candidate_buffer"`
	Results         []ScheduledQuery `json:"results"`
}

// TryPlan applies immediate worker-budget backpressure. It never queues or
// drops silently: a rejected admission is returned as E_OPT_WORKER_BACKPRESSURE.
func (s *BoundedScheduler) TryPlan(p *Project, est *EstimateReport, queryID string, bindings BindingSet) ScheduledQuery {
	q := p.Index().Queries[queryID]
	ordinal := 0
	if q != nil {
		ordinal = q.Ordinal
	}
	if !s.TryAcquire() {
		e := newError(CodeWorkerBackpressure, "worker budget is exhausted; retry after a slot is released").
			with("query_id", queryID).with("workers", s.workers).with("retryable", true)
		return ScheduledQuery{Ordinal: ordinal, QueryID: queryID, Admitted: false, ErrorCode: e.Code, ErrorMessage: e.Message}
	}
	defer s.Release()
	plan, err := PlanQuery(p, est, queryID, bindings, OptimizeOptions{WorkerBudget: s.workers, CandidateBuffer: s.buffer})
	return scheduledFromPlan(ordinal, queryID, true, plan, err)
}

// RunParallel admits all queries with blocking budget backpressure, then
// returns results in declared query ordinal order.
func RunParallel(p *Project, est *EstimateReport, bindings BindingSet, opts OptimizeOptions) (*ParallelReport, error) {
	opts = opts.withProjectDefaults(p)
	sched, err := NewBoundedScheduler(opts.WorkerBudget, opts.CandidateBuffer)
	if err != nil {
		return nil, err
	}
	_ = p.Index()
	queries := append([]Query{}, p.Queries...)
	results := make([]ScheduledQuery, len(queries))
	var wg sync.WaitGroup
	for i, q := range queries {
		i, q := i, q
		wg.Add(1)
		go func() {
			defer wg.Done()
			sched.Acquire()
			defer sched.Release()
			plan, perr := PlanQuery(p, est, q.ID, bindings, opts)
			results[i] = scheduledFromPlan(q.Ordinal, q.ID, true, plan, perr)
		}()
	}
	wg.Wait()
	return &ParallelReport{WorkerBudget: opts.WorkerBudget, CandidateBuffer: opts.CandidateBuffer, Results: results}, nil
}

func scheduledFromPlan(ordinal int, queryID string, admitted bool, plan *QueryPlan, err error) ScheduledQuery {
	r := ScheduledQuery{Ordinal: ordinal, QueryID: queryID, Admitted: admitted, Plan: plan}
	if err != nil {
		if coded, ok := AsError(err); ok {
			r.ErrorCode = coded.Code
			r.ErrorMessage = coded.Message
		} else {
			r.ErrorCode = CodeInputInvalid
			r.ErrorMessage = err.Error()
		}
	}
	if plan != nil && r.ErrorCode == "" {
		r.ErrorCode = plan.ErrorCode
		r.ErrorMessage = plan.ErrorMessage
	}
	return r
}
