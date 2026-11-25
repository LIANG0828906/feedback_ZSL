// Package parallel runs independent connection kernels under an explicit
// worker and per-stream buffer budget.
package parallel

import (
	"sort"
	"sync"

	"multiplexgw/internal/bizerr"
	"multiplexgw/internal/engine"
	"multiplexgw/internal/model"
)

const (
	MinWorkers = 1
	MaxWorkers = 64
)

type Config struct {
	Workers          int `json:"workers"`
	MaxPendingFrames int `json:"max_pending_frames"`
}

func (c Config) Validate() error {
	if c.Workers < MinWorkers || c.Workers > MaxWorkers {
		return bizerr.New(bizerr.BudgetRange, "parallel.workers",
			"worker budget %d outside [%d,%d]", c.Workers, MinWorkers, MaxWorkers).
			WithContext("min", MinWorkers).WithContext("max", MaxWorkers)
	}
	if c.MaxPendingFrames < 1 || c.MaxPendingFrames > 4096 {
		return bizerr.New(bizerr.BudgetRange, "parallel.max_pending_frames",
			"buffer budget %d outside [1,4096]", c.MaxPendingFrames).
			WithContext("min", 1).WithContext("max", 4096)
	}
	return nil
}

type Job struct {
	ConnectionID string         `json:"connection_id"`
	Project      *model.Project `json:"-"`
}

type Report struct {
	ConnectionID string             `json:"connection_id"`
	WorkerSlot   int                `json:"-"`
	Result       *model.RunResult   `json:"result,omitempty"`
	Code         string             `json:"code"`
	Error        *model.ErrorReport `json:"error,omitempty"`
}

// Pool is a countable worker budget. TryAcquire gives deterministic backpressure
// instead of unbounded goroutine admission.
type Pool struct {
	slots chan struct{}
	max   int
}

func NewPool(workers int) (*Pool, error) {
	if workers < MinWorkers || workers > MaxWorkers {
		return nil, bizerr.New(bizerr.BudgetRange, "parallel.workers", "worker budget %d outside [%d,%d]", workers, MinWorkers, MaxWorkers)
	}
	return &Pool{slots: make(chan struct{}, workers), max: workers}, nil
}

func (p *Pool) Max() int { return p.max }

func (p *Pool) TryAcquire() (func(), bool) {
	select {
	case p.slots <- struct{}{}:
		return func() { <-p.slots }, true
	default:
		return nil, false
	}
}

// RunConnections starts exactly cfg.Workers consumers and returns reports in
// lexicographic connection order.
func RunConnections(jobs []Job, cfg Config) []Report {
	if err := cfg.Validate(); err != nil {
		be, _ := bizerr.AsError(err)
		report := Report{Code: bizerr.CodeOf(err)}
		if be != nil {
			report.Error = &model.ErrorReport{Code: be.Code, Path: be.Path, Message: be.Message, Context: be.Context}
		}
		return []Report{report}
	}
	type indexedJob struct {
		index int
		job   Job
	}
	in := make(chan indexedJob)
	out := make(chan Report, len(jobs))
	var wg sync.WaitGroup
	for w := 0; w < cfg.Workers; w++ {
		wg.Add(1)
		go func(slot int) {
			defer wg.Done()
			for item := range in {
				opts := engine.Options{MaxPendingFrames: cfg.MaxPendingFrames}
				res, err := engine.Run(item.job.Project, opts)
				rep := Report{ConnectionID: item.job.ConnectionID, WorkerSlot: slot, Result: res, Code: bizerr.OK}
				if err != nil {
					rep.Code = bizerr.CodeOf(err)
					be, ok := bizerr.AsError(err)
					if ok {
						rep.Error = &model.ErrorReport{Code: be.Code, Path: be.Path, Message: be.Message, Context: be.Context}
					}
				}
				out <- rep
			}
		}(w)
	}
	go func() {
		for i, j := range jobs {
			in <- indexedJob{index: i, job: j}
		}
		close(in)
		wg.Wait()
		close(out)
	}()
	reports := make([]Report, 0, len(jobs))
	for r := range out {
		reports = append(reports, r)
	}
	sort.Slice(reports, func(i, j int) bool { return reports[i].ConnectionID < reports[j].ConnectionID })
	return reports
}

// DeterministicBackpressureProbe occupies all pool slots, then verifies that a
// second admission receives E_THREAD_BUDGET_EXHAUSTED.
func DeterministicBackpressureProbe(workers int) (string, map[string]any, error) {
	pool, err := NewPool(workers)
	if err != nil {
		return "", nil, err
	}
	releases := make([]func(), 0, workers)
	for i := 0; i < workers; i++ {
		release, ok := pool.TryAcquire()
		if !ok {
			return "", nil, bizerr.New(bizerr.ThreadBudgetExhausted, "parallel.pool", "unexpected admission failure at slot %d", i)
		}
		releases = append(releases, release)
	}
	_, ok := pool.TryAcquire()
	for _, release := range releases {
		release()
	}
	if ok {
		return "", nil, bizerr.New(bizerr.Internal, "parallel.pool", "budget probe unexpectedly admitted extra worker")
	}
	return bizerr.ThreadBudgetExhausted, map[string]any{
		"workers": workers,
		"path":    "parallel.pool.TryAcquire",
	}, nil
}
