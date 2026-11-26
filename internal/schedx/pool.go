// Package schedx provides the bounded deterministic chunk worker pool.
package schedx

import (
	"context"
	"fmt"
	"sync"
	"time"

	"acousticfield/internal/acoustic"
)

type Job struct {
	ChunkID string
	Run     func(workerID, pathBufferCap int) error
}

type queuedJob struct {
	index       int
	job         Job
	attemptedAt time.Time
	blocked     bool
}

type Pool struct {
	MaxParallel int
	PathBuffer  int
	QueueBudget int
	Now         func() time.Time
}

func DefaultPool(b acoustic.Budget) *Pool {
	return &Pool{
		MaxParallel: b.MaxParallel,
		PathBuffer:  b.PathBufferPerWorker,
		QueueBudget: b.MaxQueuedChunks,
		Now:         time.Now,
	}
}

// Run executes jobs and returns per-job errors in job order plus queue diagnostics.
func (p *Pool) Run(ctx context.Context, jobs []Job) ([]error, []acoustic.QueueRecord) {
	if p.MaxParallel <= 0 {
		p.MaxParallel = 1
	}
	if p.QueueBudget <= 0 {
		p.QueueBudget = 1
	}
	if p.Now == nil {
		p.Now = time.Now
	}
	results := make([]error, len(jobs))
	records := make([]acoustic.QueueRecord, 0, len(jobs))
	var recordMu sync.Mutex

	work := make(chan queuedJob, p.QueueBudget)
	var wg sync.WaitGroup
	for workerID := 0; workerID < p.MaxParallel; workerID++ {
		wg.Add(1)
		go func(workerID int) {
			defer wg.Done()
			for qj := range work {
				waitMS := p.Now().Sub(qj.attemptedAt).Milliseconds()
				rec := acoustic.QueueRecord{
					ChunkID:    qj.job.ChunkID,
					WorkerID:   workerID,
					Blocked:    qj.blocked,
					WaitMillis: waitMS,
				}
				if qj.blocked {
					rec.Reason = "pending_chunk_queue_budget_exhausted"
				} else if len(work) >= p.QueueBudget {
					rec.Reason = "queue_near_budget"
				}
				recordMu.Lock()
				records = append(records, rec)
				recordMu.Unlock()

				func() {
					defer func() {
						if r := recover(); r != nil {
							results[qj.index] = acoustic.NewError(acoustic.CodeBudget, qj.job.ChunkID, "worker_panic", "%v", r)
						}
					}()
					if qj.job.Run != nil {
						results[qj.index] = qj.job.Run(workerID, p.PathBuffer)
					}
				}()
			}
		}(workerID)
	}

dispatch:
	for i, job := range jobs {
		qj := queuedJob{index: i, job: job, attemptedAt: p.Now()}
		select {
		case work <- qj:
		default:
			qj.blocked = true
			select {
			case work <- qj:
			case <-ctx.Done():
				results[i] = ctx.Err()
				break dispatch
			}
		}
	}
	close(work)
	wg.Wait()
	return results, records
}

// ValidateBudget returns E_BUDGET for unusable scheduler settings.
func ValidateBudget(b acoustic.Budget) error {
	if b.MaxParallel < 1 {
		return acoustic.NewError(acoustic.CodeBudget, "scheduler", "max_parallel", "must be >= 1")
	}
	if b.PathBufferPerWorker < 1 {
		return acoustic.NewError(acoustic.CodeBudget, "scheduler", "path_buffer_per_worker", "must be >= 1")
	}
	if b.MaxQueuedChunks < 1 {
		return acoustic.NewError(acoustic.CodeBudget, "scheduler", "max_queued_chunks", "must be >= 1")
	}
	if b.MaxParallel > 64 {
		return acoustic.NewError(acoustic.CodeBudget, "scheduler", "max_parallel", fmt.Sprintf("must be <= 64"))
	}
	return nil
}
