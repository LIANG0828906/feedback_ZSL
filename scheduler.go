package acousticfield

import (
	"context"
	"fmt"
	"sort"
	"sync"
	"time"
)

type WorkItem struct {
	ChunkID int
	Run     func(ctx context.Context, workerID int) error
}

type QueueEvent struct {
	ChunkID   int    `json:"chunk_id"`
	Reason    string `json:"reason"`
	WaitNanos int64  `json:"wait_nanos"`
	WaitText  string `json:"wait_text"`
}

type WorkError struct {
	ChunkID  int    `json:"chunk_id"`
	WorkerID int    `json:"worker_id"`
	Message  string `json:"message"`
	Err      error  `json:"-"`
}

func (e WorkError) Error() string {
	return fmt.Sprintf("chunk-%d worker-%d: %v", e.ChunkID, e.WorkerID, e.Err)
}

type RunSummary struct {
	Errors      []WorkError  `json:"errors"`
	QueueEvents []QueueEvent `json:"queue_events"`
	Completed   []int        `json:"completed"`
}

// ParallelScheduler bounds worker count and the pending FIFO. When the queue
// budget is exhausted submission blocks and a wait event is emitted.
type ParallelScheduler struct {
	MaxWorkers  int
	QueueBudget int
}

func NewParallelScheduler(maxWorkers, queueBudget int) *ParallelScheduler {
	if maxWorkers <= 0 {
		maxWorkers = 1
	}
	if queueBudget <= 0 {
		queueBudget = 1
	}
	return &ParallelScheduler{MaxWorkers: maxWorkers, QueueBudget: queueBudget}
}

func (s *ParallelScheduler) Run(ctx context.Context, jobs []WorkItem) RunSummary {
	jobCh := make(chan WorkItem, s.QueueBudget)
	var mu sync.Mutex
	summary := RunSummary{}
	resultCh := make(chan struct{}, len(jobs))
	var wg sync.WaitGroup
	for workerID := 0; workerID < s.MaxWorkers; workerID++ {
		wg.Add(1)
		go func(id int) {
			defer wg.Done()
			for job := range jobCh {
				err := runIsolated(ctx, id, job)
				mu.Lock()
				if err != nil {
					summary.Errors = append(summary.Errors, WorkError{ChunkID: job.ChunkID, WorkerID: id, Message: err.Error(), Err: err})
				} else {
					summary.Completed = append(summary.Completed, job.ChunkID)
				}
				mu.Unlock()
				resultCh <- struct{}{}
			}
		}(workerID)
	}

	submitted := 0
submitLoop:
	for _, job := range jobs {
		if err := ctx.Err(); err != nil {
			break
		}
		start := time.Now()
		select {
		case jobCh <- job:
			wait := time.Since(start)
			if wait > 2*time.Millisecond {
				summary.QueueEvents = append(summary.QueueEvents, QueueEvent{
					ChunkID: job.ChunkID, Reason: "worker_saturation",
					WaitNanos: wait.Nanoseconds(), WaitText: wait.String(),
				})
			}
			submitted++
		default:
			select {
			case jobCh <- job:
				wait := time.Since(start)
				summary.QueueEvents = append(summary.QueueEvents, QueueEvent{
					ChunkID: job.ChunkID, Reason: "queue_budget_exhausted",
					WaitNanos: wait.Nanoseconds(), WaitText: wait.String(),
				})
				submitted++
			case <-ctx.Done():
				break submitLoop
			}
		}
	}
	close(jobCh)
	wg.Wait()
	close(resultCh)
	// Drain completion notifications. There are no goroutines left, so the
	// channel merely carries already-written values.
	for range resultCh {
	}
	for _, job := range jobs[submitted:] {
		summary.Errors = append(summary.Errors, WorkError{ChunkID: job.ChunkID, WorkerID: -1, Message: ctx.Err().Error(), Err: ctx.Err()})
	}
	sortWorkErrors(summary.Errors)
	sort.Ints(summary.Completed)
	sortQueueEvents(summary.QueueEvents)
	return summary
}

func runIsolated(ctx context.Context, workerID int, job WorkItem) (err error) {
	defer func() {
		if r := recover(); r != nil {
			err = &FieldError{
				Code: CodeState, ObjectID: fmt.Sprintf("chunk-%d", job.ChunkID),
				FieldPath: "worker", Message: fmt.Sprintf("worker panic isolated: %v", r),
			}
		}
	}()
	if err := ctx.Err(); err != nil {
		return err
	}
	return job.Run(ctx, workerID)
}

func sortWorkErrors(v []WorkError) {
	sort.Slice(v, func(i, j int) bool {
		if v[i].ChunkID != v[j].ChunkID {
			return v[i].ChunkID < v[j].ChunkID
		}
		return v[i].WorkerID < v[j].WorkerID
	})
}

func sortQueueEvents(v []QueueEvent) {
	sort.SliceStable(v, func(i, j int) bool {
		if v[i].ChunkID != v[j].ChunkID {
			return v[i].ChunkID < v[j].ChunkID
		}
		return v[i].WaitNanos > v[j].WaitNanos
	})
}
