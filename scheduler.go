package maskqc

import (
	"context"
	"fmt"
	"sync"
	"time"
)

// SchedulerOptions bounds parallelism, per-worker pixels, and pending jobs.
type SchedulerOptions struct {
	MaxParallel     int `json:"max_parallel"`
	MaxBufferPixels int `json:"max_buffer_pixels"`
	QueueBudget     int `json:"queue_budget"`
}

func DefaultSchedulerOptions() SchedulerOptions {
	return SchedulerOptions{MaxParallel: 2, MaxBufferPixels: 4096, QueueBudget: 64}
}

func (o SchedulerOptions) normalized(jobCount int) SchedulerOptions {
	if o.MaxParallel <= 0 {
		o.MaxParallel = 2
	}
	if jobCount > 0 && o.MaxParallel > jobCount {
		o.MaxParallel = jobCount
	}
	if o.MaxBufferPixels <= 0 {
		o.MaxBufferPixels = 256
	}
	if o.QueueBudget <= 0 {
		o.QueueBudget = o.MaxParallel
	}
	return o
}

// Job is one isolated chunk raster/accumulation task.
type Job struct {
	ID     string
	Chunk  ChunkCoord
	Pixels int
	Run    func(pixelBuffer []byte) error
}

// JobResult captures failure without affecting unrelated jobs.
type JobResult struct {
	Index    int          `json:"index"`
	ID       string       `json:"id"`
	Chunk    ChunkCoord   `json:"chunk"`
	Attempts int          `json:"attempts"`
	Err      *Error       `json:"error,omitempty"`
	Waits    []WaitRecord `json:"waits,omitempty"`
}

type queuedJob struct {
	job              Job
	index            int
	blockedEnqueueAt time.Time
}

// RunJobs executes jobs with bounded workers and a bounded pending queue.
// Results are returned in input order; completion order does not affect them.
func RunJobs(ctx context.Context, jobs []Job, opts SchedulerOptions) []JobResult {
	opts = opts.normalized(len(jobs))
	results := make([]JobResult, len(jobs))
	if len(jobs) == 0 {
		return results
	}

	queue := make(chan queuedJob, opts.QueueBudget)
	var wg sync.WaitGroup
	for worker := 0; worker < opts.MaxParallel; worker++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			pixelBuffer := make([]byte, opts.MaxBufferPixels)
			for qj := range queue {
				result := executeJob(ctx, qj.job, qj.index, pixelBuffer, opts)
				if !qj.blockedEnqueueAt.IsZero() {
					result.Waits = append(result.Waits, WaitRecord{
						Chunk: qj.job.Chunk, Reason: "queue_budget_and_pixel_buffers_exhausted",
						WaitMillis:  int64(time.Since(qj.blockedEnqueueAt) / time.Millisecond),
						Parallelism: opts.MaxParallel, QueueBudget: opts.QueueBudget,
					})
				}
				results[qj.index] = result
			}
		}()
	}

	for i, job := range jobs {
		if job.Pixels > opts.MaxBufferPixels {
			results[i] = JobResult{Index: i, ID: job.ID, Chunk: job.Chunk, Attempts: 0, Err: paramError("scheduler.pixels", 0, opts.MaxBufferPixels, "chunk exceeds per-thread pixel buffer")}
			continue
		}
		qj := queuedJob{job: job, index: i}
		if len(queue) == cap(queue) {
			qj.blockedEnqueueAt = time.Now()
			select {
			case queue <- qj:
			case <-ctx.Done():
				results[i] = JobResult{Index: i, ID: job.ID, Chunk: job.Chunk, Err: &Error{Code: CodeState, Field: "context", Message: ctx.Err().Error()}}
				continue
			}
		} else {
			select {
			case queue <- qj:
			case <-ctx.Done():
				results[i] = JobResult{Index: i, ID: job.ID, Chunk: job.Chunk, Err: &Error{Code: CodeState, Field: "context", Message: ctx.Err().Error()}}
				continue
			}
		}
	}
	close(queue)
	wg.Wait()
	return results
}

func executeJob(ctx context.Context, job Job, index int, pixelBuffer []byte, opts SchedulerOptions) (res JobResult) {
	res = JobResult{Index: index, ID: job.ID, Chunk: job.Chunk}
	defer func() {
		if r := recover(); r != nil {
			res.Err = &Error{Code: CodeState, Field: job.ID, Message: fmt.Sprintf("isolated job panic: %v", r)}
		}
	}()
	if err := ctx.Err(); err != nil {
		res.Err = &Error{Code: CodeState, Field: "context", Message: err.Error()}
		return res
	}
	res.Attempts = 1
	if job.Run == nil {
		return res
	}
	if err := job.Run(pixelBuffer); err != nil {
		if pe, ok := AsError(err); ok {
			res.Err = pe
		} else {
			res.Err = &Error{Code: CodeState, Field: job.ID, Message: err.Error()}
		}
	}
	return res
}

// RetryFailed reruns only failed jobs. A transient failure does not affect
// previously successful chunks or already committed checkpoints.
func RetryFailed(ctx context.Context, jobs []Job, results []JobResult, opts SchedulerOptions) []JobResult {
	failedJobs := []Job{}
	failedIndexes := []int{}
	for i, r := range results {
		if r.Err != nil && i < len(jobs) {
			failedJobs = append(failedJobs, jobs[i])
			failedIndexes = append(failedIndexes, i)
		}
	}
	if len(failedJobs) == 0 {
		return results
	}
	retryResults := RunJobs(ctx, failedJobs, opts)
	for j, rr := range retryResults {
		idx := failedIndexes[j]
		rr.Index = idx
		rr.Attempts = results[idx].Attempts + rr.Attempts
		results[idx] = rr
	}
	return results
}

// AllJobsFailed returns the first job error if any.
func AllJobsFailed(results []JobResult) *Error {
	for _, r := range results {
		if r.Err != nil {
			return r.Err
		}
	}
	return nil
}
