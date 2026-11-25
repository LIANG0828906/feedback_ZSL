// Package engine drives the shared fncore state machines in parallel. A
// bounded worker semaphore and per-function bounded buffers make resource
// exhaustion observable as stable backpressure codes; determinism is
// guaranteed because each function owns an independent core and RNG stream.
package engine

import (
	"sync"
	"time"

	"coldkernel/internal/errcode"
	"coldkernel/internal/fncore"
	"coldkernel/internal/model"
	"coldkernel/internal/oracle"
)

// Config declares the resource budget for a parallel run.
type Config struct {
	Workers       int // goroutine budget (>=1)
	FuncBuffer    int // per-function pending work buffer (>=1)
	SubmitBatch   int // arrivals submitted per batch (>=1)
}

// Timing is a diagnostic-only per-function wall-time observation. It must
// never be used as a correctness signal.
type Timing struct {
	FunctionID string
	Nanos      int64
}

// Run executes the project under the given resource budget and returns the
// same RunResult shape as the oracle.
func Run(p *model.Project, cfg Config) (*model.RunResult, []Timing, error) {
	if cfg.Workers < 1 {
		return nil, nil, errcode.New(errcode.EBackpressure, "worker budget must be >= 1", "workers", cfg.Workers)
	}
	if cfg.FuncBuffer < 1 {
		return nil, nil, errcode.New(errcode.EBackpressure, "function buffer must be >= 1", "buffer", cfg.FuncBuffer)
	}
	if cfg.SubmitBatch < 1 {
		cfg.SubmitBatch = 1
	}

	cores := map[string]*fncore.Core{}
	for _, fn := range p.Functions {
		cores[fn.ID] = fncore.New(fn, p.Seed)
	}

	arrivals := map[int64]map[string][]*model.Request{}
	for _, r := range p.Requests {
		if arrivals[r.ArriveTick] == nil {
			arrivals[r.ArriveTick] = map[string][]*model.Request{}
		}
		arrivals[r.ArriveTick][r.FunctionID] =
			append(arrivals[r.ArriveTick][r.FunctionID], r)
	}

	out := &model.RunResult{ByRequest: map[string]*model.RequestOutcome{}, ToleranceOK: true}
	timings := make([]Timing, 0, len(p.Functions))

	for tick := int64(0); tick < p.HorizonTicks; tick++ {
		tr, t, err := runTick(p, cores, arrivals[tick], tick, cfg)
		if err != nil {
			return nil, nil, err
		}
		timings = append(timings, t...)
		for _, oc := range allOutcomes(tr) {
			out.ByRequest[oc.RequestID] = oc
		}
		out.Ticks = append(out.Ticks, tr)
	}
	return out, timings, nil
}

type tickWork struct {
	fnID string
	reqs []*model.Request
}

type tickResult struct {
	fnID     string
	tr       *model.TickResult
	snapshot *model.FunctionTickState
	timing   Timing
}

func runTick(p *model.Project, cores map[string]*fncore.Core,
	arrivals map[string][]*model.Request, tick int64, cfg Config,
) (*model.TickResult, []Timing, error) {
	// Bounded work queue: the channel capacity is the declared per-function
	// buffer budget. A non-blocking send that fails means the budget is
	// exhausted, so we return a stable code instead of growing memory or
	// silently dropping the request.
	workCh := make(chan tickWork, cfg.FuncBuffer)
	resultCh := make(chan tickResult, len(p.Functions))

	var wg sync.WaitGroup
	// A persistent, bounded worker pool: workers are reused across function
	// items, so Workers=1 still processes every function sequentially.
	for w := 0; w < cfg.Workers; w++ {
		wg.Add(1)
		go worker(&wg, workCh, resultCh, cores, tick)
	}

	// Submit function work in batches, honouring both the worker semaphore
	// and the bounded channel buffer. Each function is one work item.
	var pending []tickWork
	for _, fn := range p.Functions {
		pending = append(pending, tickWork{fnID: fn.ID, reqs: arrivals[fn.ID]})
	}
	for start := 0; start < len(pending); start += cfg.SubmitBatch {
		end := start + cfg.SubmitBatch
		if end > len(pending) {
			end = len(pending)
		}
		for _, w := range pending[start:end] {
			// Non-blocking enqueue: a full bounded buffer is the only
			// backpressure source and yields a stable observable code.
			select {
			case workCh <- w:
			default:
				close(workCh)
				wg.Wait()
				return nil, nil, errcode.New(errcode.EBackpressure,
					"function buffer exhausted",
					"buffer", cfg.FuncBuffer, "function", w.fnID)
			}
		}
	}

	close(workCh)
	wg.Wait()
	close(resultCh)

	merged := &model.TickResult{Tick: tick, Functions: map[string]*model.FunctionTickState{}}
	var timings []Timing
	for res := range resultCh {
		merged.Admitted = append(merged.Admitted, res.tr.Admitted...)
		merged.Queued = append(merged.Queued, res.tr.Queued...)
		merged.Rejected = append(merged.Rejected, res.tr.Rejected...)
		merged.Functions[res.fnID] = res.snapshot
		timings = append(timings, res.timing)
	}
	oracle.SortOutcomes(merged.Admitted)
	oracle.SortOutcomes(merged.Queued)
	oracle.SortOutcomes(merged.Rejected)
	sortTimings(timings)
	return merged, timings, nil
}

func worker(wg *sync.WaitGroup, workCh <-chan tickWork, resultCh chan<- tickResult,
	cores map[string]*fncore.Core, tick int64) {
	defer wg.Done()
	for w := range workCh {
		c := cores[w.fnID]
		start := time.Now()
		local := &model.TickResult{Tick: tick}
		c.BeginTick(tick)
		local.Rejected = append(local.Rejected, c.ExpireQueue(tick)...)
		adm, rej := c.DrainQueue(tick)
		local.Admitted = append(local.Admitted, adm...)
		local.Rejected = append(local.Rejected, rej...)
		for _, r := range w.reqs {
			oc := c.HandleArrival(r, tick)
			record(local, oc)
		}
		elapsed := time.Since(start).Nanoseconds()
		resultCh <- tickResult{
			fnID:     w.fnID,
			tr:       local,
			snapshot: c.Snapshot(tick),
			timing:   Timing{FunctionID: w.fnID, Nanos: elapsed},
		}
	}
}

func record(tr *model.TickResult, oc *model.RequestOutcome) {
	switch oc.Decision {
	case model.DecisionAdmit:
		tr.Admitted = append(tr.Admitted, oc)
	case model.DecisionQueue:
		tr.Queued = append(tr.Queued, oc)
	default:
		tr.Rejected = append(tr.Rejected, oc)
	}
}

func allOutcomes(tr *model.TickResult) []*model.RequestOutcome {
	var all []*model.RequestOutcome
	all = append(all, tr.Admitted...)
	all = append(all, tr.Queued...)
	all = append(all, tr.Rejected...)
	return all
}

func sortTimings(ts []Timing) {
	for i := 1; i < len(ts); i++ {
		for j := i; j > 0 && ts[j-1].FunctionID > ts[j].FunctionID; j-- {
			ts[j], ts[j-1] = ts[j-1], ts[j]
		}
	}
}
