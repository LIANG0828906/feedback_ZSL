package leasekit

import (
	"fmt"
	"sort"
	"sync"
	"time"
)

const (
	MaxWorkerBudget      = 64
	MaxAttemptBudget     = 100000
	DefaultWorkerBudget  = 4
	DefaultAttemptBudget = 8
)

type ParallelOptions struct {
	Workers                  int
	AttemptBudgetPerResource int
	BatchSize                int
	ReverseSubmission        bool
}

type ResourceObservation struct {
	ResourceID         string        `json:"resourceId"`
	HolderCount        int           `json:"holderCount"`
	OperationCount     int           `json:"operationCount"`
	RenewalCount       int           `json:"renewalCount"`
	AttemptBudget      int           `json:"attemptBudget"`
	ElapsedNanoseconds int64         `json:"elapsedNanoseconds"`
	Elapsed            time.Duration `json:"-"`
}

type ParallelReport struct {
	Report       *Report               `json:"report"`
	Observations []ResourceObservation `json:"observations"`
	Workers      int                   `json:"workers"`
	BatchSize    int                   `json:"batchSize"`
}

func ValidateParallelOptions(opts ParallelOptions) (ParallelOptions, error) {
	if opts.Workers == 0 {
		opts.Workers = DefaultWorkerBudget
	}
	if opts.Workers < 0 || opts.Workers > MaxWorkerBudget {
		return opts, validationError(CodeWorkerBudgetOOR, "options.workers", "worker budget must be in [1,%d], got %d", MaxWorkerBudget, opts.Workers)
	}
	if opts.AttemptBudgetPerResource == 0 {
		opts.AttemptBudgetPerResource = DefaultAttemptBudget
	}
	if opts.AttemptBudgetPerResource < 0 || opts.AttemptBudgetPerResource > MaxAttemptBudget {
		return opts, validationError(CodeAttemptBudgetOOR, "options.attemptBudgetPerResource", "attempt budget must be in [1,%d], got %d", MaxAttemptBudget, opts.AttemptBudgetPerResource)
	}
	if opts.BatchSize < 0 || opts.BatchSize > 1024 {
		return opts, validationError(CodeFieldOutOfRange, "options.batchSize", "batch size out of range: %d", opts.BatchSize)
	}
	return opts, nil
}

func RunParallel(p *Project, opts ParallelOptions) (*ParallelReport, error) {
	if err := ValidateProject(p); err != nil {
		return nil, err
	}
	opts, err := ValidateParallelOptions(opts)
	if err != nil {
		return nil, err
	}

	resourceIDs := make([]string, 0, len(p.Resources))
	for _, r := range p.Resources {
		resourceIDs = append(resourceIDs, r.ID)
	}
	sort.Strings(resourceIDs)
	if opts.ReverseSubmission {
		for i, j := 0, len(resourceIDs)-1; i < j; i, j = i+1, j-1 {
			resourceIDs[i], resourceIDs[j] = resourceIDs[j], resourceIDs[i]
		}
	}

	runOpts := RunOptions{AttemptBudgetPerResource: opts.AttemptBudgetPerResource}
	reports := map[string]*Report{}
	observations := map[string]ResourceObservation{}
	var mu sync.Mutex

	batchSize := opts.BatchSize
	if batchSize <= 0 || batchSize > len(resourceIDs) {
		batchSize = len(resourceIDs)
	}
	for start := 0; start < len(resourceIDs); start += batchSize {
		end := start + batchSize
		if end > len(resourceIDs) {
			end = len(resourceIDs)
		}
		batch := append([]string(nil), resourceIDs[start:end]...)
		jobs := make(chan string)
		var wg sync.WaitGroup
		workerCount := opts.Workers
		if workerCount > len(batch) {
			workerCount = len(batch)
		}
		if workerCount < 1 {
			workerCount = 1
		}
		for i := 0; i < workerCount; i++ {
			wg.Add(1)
			go func() {
				defer wg.Done()
				for rid := range jobs {
					startAt := time.Now()
					rr, rerr := runResourceWithOptions(p, rid, runOpts)
					elapsed := time.Since(startAt)
					mu.Lock()
					if rerr == nil {
						reports[rid] = rr
					}
					obs := observations[rid]
					obs.ResourceID = rid
					obs.ElapsedNanoseconds += int64(elapsed)
					obs.Elapsed += elapsed
					observations[rid] = obs
					mu.Unlock()
					if rerr != nil {
						continue
					}
				}
			}()
		}
		for _, rid := range batch {
			jobs <- rid
		}
		close(jobs)
		wg.Wait()
	}

	for _, rid := range resourceIDs {
		if _, ok := reports[rid]; !ok {
			return nil, fmt.Errorf("parallel resource %s did not produce a report", rid)
		}
	}

	merged := mergeResourceReports(p, reports)
	obsList := make([]ResourceObservation, 0, len(resourceIDs))
	for _, rid := range resourceIDs {
		obs := observations[rid]
		obs.AttemptBudget = opts.AttemptBudgetPerResource
		for _, h := range p.Holders {
			if h.ResourceID == rid {
				obs.HolderCount++
			}
		}
		for _, op := range p.Operations {
			h := holderByID(p, op.HolderID)
			if h != nil && h.ResourceID == rid {
				obs.OperationCount++
				if op.Kind == OpRenew {
					obs.RenewalCount++
				}
			}
		}
		obsList = append(obsList, obs)
	}
	return &ParallelReport{Report: merged, Observations: obsList, Workers: opts.Workers, BatchSize: batchSize}, nil
}

func mergeResourceReports(p *Project, reports map[string]*Report) *Report {
	merged := &Report{
		SchemaVersion: CanonicalSchemaVersion,
		ProjectID:     p.ProjectID,
		Seed:          p.Seed,
		ToleranceMS:   0,
	}
	for _, op := range p.SortedOperations() {
		h := holderByID(p, op.HolderID)
		if h == nil {
			continue
		}
		rr := reports[h.ResourceID]
		if rr == nil {
			continue
		}
		if result := rr.ResultBySeq(op.Seq); result != nil {
			merged.Results = append(merged.Results, *result)
		}
	}
	for _, rr := range reports {
		merged.Events = append(merged.Events, rr.Events...)
	}
	sort.SliceStable(merged.Events, func(i, j int) bool {
		a, b := merged.Events[i], merged.Events[j]
		if a.AtMS != b.AtMS {
			return a.AtMS < b.AtMS
		}
		if a.ResourceID != b.ResourceID {
			return a.ResourceID < b.ResourceID
		}
		if a.HolderID != b.HolderID {
			return a.HolderID < b.HolderID
		}
		rank := map[string]int{"lease_invalidated": 0, "resource_released": 1}
		if rank[a.Type] != rank[b.Type] {
			return rank[a.Type] < rank[b.Type]
		}
		return a.Type < b.Type
	})
	for i := range merged.Events {
		merged.Events[i].Order = i + 1
	}
	for _, h := range p.Holders {
		for _, rr := range reports {
			for _, st := range rr.HolderStates {
				if st.HolderID == h.ID {
					merged.HolderStates = append(merged.HolderStates, st)
				}
			}
		}
	}
	return merged
}
