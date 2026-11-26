// Package scheduler runs bounded parallel part preparation without making
// batch results concurrency-dependent.
package scheduler

import (
	"context"
	"sort"
	"sync"
	"time"

	"wardrobekernel/internal/hashx"
	"wardrobekernel/internal/model"
)

// Options configures the bounded preparation pool.
type Options struct {
	MaxParallelism           int
	CandidateBufferPerWorker int
	PendingQueueBudget       int
	WorkDelay                time.Duration
	FailFirstAttempt         map[string]bool
	PersistentFail           map[string]bool
}

// PreparedPart is a worker result before deterministic batch reduction.
type PreparedPart struct {
	Part     model.Part `json:"part"`
	Hash     string     `json:"hash"`
	Attempts int        `json:"attempts"`
}

// Result contains prepared parts and runtime pressure diagnostics.
type Result struct {
	Prepared []PreparedPart      `json:"prepared"`
	Waits    []model.WaitRecord  `json:"waits"`
	Failures []model.PartFailure `json:"failures"`
}

// Prepare processes ids with bounded resources. It never mutates the project.
func Prepare(ctx context.Context, project *model.Project, ids []string, opts Options) (*Result, error) {
	if opts.MaxParallelism <= 0 {
		opts.MaxParallelism = project.Constraints.Scheduler.MaxParallelism
	}
	if opts.CandidateBufferPerWorker <= 0 {
		opts.CandidateBufferPerWorker = project.Constraints.Scheduler.CandidateBufferPerWorker
	}
	if opts.PendingQueueBudget <= 0 {
		opts.PendingQueueBudget = project.Constraints.Scheduler.PendingQueueBudget
	}
	if opts.MaxParallelism <= 0 {
		opts.MaxParallelism = 2
	}
	if opts.CandidateBufferPerWorker <= 0 {
		opts.CandidateBufferPerWorker = 1
	}
	if opts.PendingQueueBudget <= 0 {
		opts.PendingQueueBudget = len(ids)
	}

	partByID := map[string]model.Part{}
	for _, part := range project.Parts {
		partByID[part.ID] = part
	}

	result := &Result{Prepared: []PreparedPart{}, Waits: []model.WaitRecord{}, Failures: []model.PartFailure{}}
	var mu sync.Mutex
	recordWait := func(w model.WaitRecord) {
		mu.Lock()
		result.Waits = append(result.Waits, w)
		mu.Unlock()
	}
	recordFailure := func(f model.PartFailure) {
		mu.Lock()
		result.Failures = append(result.Failures, f)
		mu.Unlock()
	}

	pending := make(chan string, opts.PendingQueueBudget)
	workerChans := make([]chan string, opts.MaxParallelism)
	var workers sync.WaitGroup
	for i := 0; i < opts.MaxParallelism; i++ {
		workerChans[i] = make(chan string, opts.CandidateBufferPerWorker)
		workers.Add(1)
		go func(ch <-chan string) {
			defer workers.Done()
			for id := range ch {
				part, ok := partByID[id]
				if !ok {
					recordFailure(model.PartFailure{PartID: id, Code: "E_SCHEMA", Message: "unknown scheduled part", Retryable: false})
					continue
				}
				prepared, failure := prepareOne(ctx, part, opts, recordFailure)
				if failure != nil {
					recordFailure(*failure)
					continue
				}
				mu.Lock()
				result.Prepared = append(result.Prepared, *prepared)
				mu.Unlock()
			}
		}(workerChans[i])
	}

	dispatchDone := make(chan struct{})
	go func() {
		for id := range pending {
			idx := chooseWorker(workerChans)
			ch := workerChans[idx]
			select {
			case ch <- id:
			default:
				start := time.Now()
				select {
				case ch <- id:
					d := time.Since(start)
					recordWait(model.WaitRecord{
						Reason: "CANDIDATE_BUFFER_FULL", PartID: id, DurationMS: d.Milliseconds(), DurationNS: d.Nanoseconds(),
						QueueDepth: len(ch), Capacity: cap(ch),
					})
				case <-ctx.Done():
					return
				}
			}
		}
		for _, ch := range workerChans {
			close(ch)
		}
		close(dispatchDone)
	}()

	for _, id := range ids {
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		default:
		}
		select {
		case pending <- id:
		default:
			start := time.Now()
			select {
			case pending <- id:
				d := time.Since(start)
				recordWait(model.WaitRecord{
					Reason: "PENDING_QUEUE_FULL", PartID: id, DurationMS: d.Milliseconds(), DurationNS: d.Nanoseconds(),
					QueueDepth: len(pending), Capacity: cap(pending),
				})
			case <-ctx.Done():
				return nil, ctx.Err()
			}
		}
	}
	close(pending)
	<-dispatchDone
	workers.Wait()

	sort.SliceStable(result.Prepared, func(i, j int) bool { return result.Prepared[i].Part.ID < result.Prepared[j].Part.ID })
	sort.SliceStable(result.Failures, func(i, j int) bool {
		if result.Failures[i].PartID != result.Failures[j].PartID {
			return result.Failures[i].PartID < result.Failures[j].PartID
		}
		return result.Failures[i].Attempt < result.Failures[j].Attempt
	})
	sort.SliceStable(result.Waits, func(i, j int) bool {
		if result.Waits[i].Reason != result.Waits[j].Reason {
			return result.Waits[i].Reason < result.Waits[j].Reason
		}
		return result.Waits[i].PartID < result.Waits[j].PartID
	})
	return result, nil
}

func chooseWorker(channels []chan string) int {
	best := 0
	bestLen := len(channels[0])
	for i := 1; i < len(channels); i++ {
		if len(channels[i]) < bestLen {
			best, bestLen = i, len(channels[i])
		}
	}
	return best
}

func prepareOne(ctx context.Context, part model.Part, opts Options, recordFailure func(model.PartFailure)) (*PreparedPart, *model.PartFailure) {
	maxAttempts := 2
	for attempt := 1; attempt <= maxAttempts; attempt++ {
		if opts.WorkDelay > 0 {
			timer := time.NewTimer(opts.WorkDelay)
			select {
			case <-timer.C:
			case <-ctx.Done():
				timer.Stop()
				return nil, &model.PartFailure{PartID: part.ID, Attempt: attempt, Code: "E_CTX", Message: ctx.Err().Error(), Retryable: true}
			}
		}
		if attempt == 1 && opts.FailFirstAttempt[part.ID] {
			continue
		}
		if opts.PersistentFail[part.ID] {
			retryable := attempt < maxAttempts
			f := model.PartFailure{
				PartID: part.ID, Attempt: attempt, Code: "E_PART_FAILED",
				Message: "injected persistent single-part failure", Retryable: retryable,
			}
			if !retryable {
				return nil, &f
			}
			recordFailure(f)
			continue
		}
		prepared := &PreparedPart{Part: part, Hash: hashx.Of(part), Attempts: attempt}
		if attempt > 1 && opts.FailFirstAttempt[part.ID] {
			recordFailure(model.PartFailure{
				PartID: part.ID, Attempt: 1, Code: "E_PART_TRANSIENT",
				Message: "injected recoverable single-part preparation failure", Retryable: true, Recovered: true,
			})
		}
		return prepared, nil
	}
	return nil, &model.PartFailure{PartID: part.ID, Attempt: maxAttempts, Code: "E_PART_FAILED", Message: "part failed after retry", Retryable: false}
}
