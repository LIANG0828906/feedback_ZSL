package strokelab

import (
	"context"
	"sync"
	"time"
)

type SchedulerConfig struct {
	MaxParallelism     int `json:"max_parallelism"`
	MaxPixelsPerWorker int `json:"max_pixels_per_worker"`
	QueueBudget        int `json:"queue_budget"`
	MaxRetries         int `json:"max_retries"`
}

func DefaultSchedulerConfig() SchedulerConfig {
	return SchedulerConfig{
		MaxParallelism:     3,
		MaxPixelsPerWorker: 4096,
		QueueBudget:        8,
		MaxRetries:         1,
	}
}

func (c SchedulerConfig) normalized() (SchedulerConfig, error) {
	if c == (SchedulerConfig{}) {
		c = DefaultSchedulerConfig()
	}
	if c.MaxParallelism <= 0 {
		c.MaxParallelism = 1
	}
	if c.MaxPixelsPerWorker <= 0 {
		return c, coded(CodeBudget, "MaxPixelsPerWorker must be positive")
	}
	if c.QueueBudget < 0 {
		return c, coded(CodeBudget, "QueueBudget must be non-negative")
	}
	if c.QueueBudget == 0 {
		// An unbuffered queue is legal but still explicit; callers that need
		// maximal throughput can raise it.
		c.QueueBudget = 0
	}
	if c.MaxRetries < 0 {
		c.MaxRetries = 0
	}
	return c, nil
}

type BlockTask struct {
	ID            string
	StrokeID      string
	StrokeOrdinal int
	BlockID       int
	Block         Rect
	Attempt       int
	Run           func() ([]uint8, error)
}

type QueueRecord struct {
	TaskID       string `json:"task_id"`
	StrokeID     string `json:"stroke_id"`
	StrokeOrder  int    `json:"stroke_ordinal"`
	BlockID      int    `json:"block_id"`
	Attempt      int    `json:"attempt"`
	QueueReason  string `json:"queue_reason"`
	QueueWaitMS  int64  `json:"queue_wait_ms"`
	BudgetReason string `json:"budget_reason"`
	BudgetWaitMS int64  `json:"budget_wait_ms"`
	Outcome      string `json:"outcome"`
	Error        string `json:"error,omitempty"`
}

type TaskResult struct {
	Task      BlockTask
	Component []uint8
	Err       error
}

type Scheduler struct {
	Config          SchedulerConfig
	FailureInjector func(task BlockTask, attempt int) error

	mu      sync.Mutex
	records []QueueRecord
}

func NewScheduler(config SchedulerConfig) (*Scheduler, error) {
	c, err := config.normalized()
	if err != nil {
		return nil, err
	}
	return &Scheduler{Config: c}, nil
}

func (s *Scheduler) Records() []QueueRecord {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]QueueRecord(nil), s.records...)
}

func (s *Scheduler) addRecord(r QueueRecord) {
	s.mu.Lock()
	s.records = append(s.records, r)
	s.mu.Unlock()
}

// RunBatch executes independent block tasks with bounded worker count, bounded
// queue depth, and a weighted per-worker pixel budget. A failed task is retried
// alone; successful results from the other blocks remain in memory but are
// never treated as a committed stroke until every task succeeds.
func (s *Scheduler) RunBatch(ctx context.Context, tasks []BlockTask) ([]TaskResult, error) {
	if len(tasks) == 0 {
		return nil, nil
	}
	pixelCap := s.Config.MaxParallelism * s.Config.MaxPixelsPerWorker
	for _, task := range tasks {
		if task.Block.PixelCount() > s.Config.MaxPixelsPerWorker {
			return nil, withDetails(CodeBudget,
				"block %d needs %d pixels above per-worker limit %d",
				map[string]any{"block_id": task.BlockID, "pixels": task.Block.PixelCount(),
					"limit": s.Config.MaxPixelsPerWorker},
				"block %d exceeds per-worker pixel budget", task.BlockID)
		}
		if task.Block.PixelCount() > pixelCap {
			return nil, coded(CodeBudget, "block %d exceeds total pixel budget", task.BlockID)
		}
	}
	results := make([]TaskResult, len(tasks))
	pending := make([]BlockTask, len(tasks))
	copy(pending, tasks)
	completed := make(map[string]TaskResult, len(tasks))
	for round := 0; round <= s.Config.MaxRetries; round++ {
		for i := range pending {
			pending[i].Attempt = round + 1
		}
		roundResults, err := s.runRound(ctx, pending, pixelCap)
		if err != nil {
			return nil, err
		}
		var retry []BlockTask
		for i, res := range roundResults {
			completed[res.Task.ID] = res
			if res.Err != nil {
				if round < s.Config.MaxRetries {
					retry = append(retry, pending[i])
				}
			}
		}
		if len(retry) == 0 {
			break
		}
		pending = retry
	}
	for i, task := range tasks {
		res, ok := completed[task.ID]
		if !ok {
			return nil, coded(CodeBudget, "task %s was not scheduled", task.ID)
		}
		if res.Err != nil {
			return results, withDetails(CodeExecution,
				"block %d stroke %s failed after retries: %v",
				map[string]any{"block_id": task.BlockID, "stroke_id": task.StrokeID,
					"attempts": s.Config.MaxRetries + 1},
				task.BlockID, task.StrokeID, res.Err)
		}
		results[i] = res
	}
	return results, nil
}

type roundTask struct {
	task BlockTask
}

func (s *Scheduler) runRound(ctx context.Context, tasks []BlockTask, pixelCap int) ([]TaskResult, error) {
	type budget struct {
		mu       sync.Mutex
		cond     *sync.Cond
		inFlight int
	}
	b := &budget{}
	b.cond = sync.NewCond(&b.mu)
	taskCh := make(chan roundTask, s.Config.QueueBudget)
	resultCh := make(chan struct {
		task    BlockTask
		comp    []uint8
		err     error
		qwait   time.Duration
		breason string
		bwait   time.Duration
	}, len(tasks))
	var wg sync.WaitGroup
	for worker := 0; worker < s.Config.MaxParallelism; worker++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for jt := range taskCh {
				task := jt.task
				if err := ctx.Err(); err != nil {
					resultCh <- struct {
						task    BlockTask
						comp    []uint8
						err     error
						qwait   time.Duration
						breason string
						bwait   time.Duration
					}{task: task, err: err}
					continue
				}
				pixels := task.Block.PixelCount()
				waitStart := time.Now()
				b.mu.Lock()
				waited := false
				if b.inFlight+pixels > pixelCap {
					waited = true
					for b.inFlight+pixels > pixelCap {
						b.cond.Wait()
					}
				}
				b.inFlight += pixels
				b.mu.Unlock()
				budgetWait := time.Duration(0)
				budgetReason := "available"
				if waited {
					budgetWait = time.Since(waitStart)
					budgetReason = "pixel_buffer_budget"
				}
				component, err := task.Run()
				if err == nil && s.FailureInjector != nil {
					err = s.FailureInjector(task, task.Attempt)
				}
				b.mu.Lock()
				b.inFlight -= pixels
				b.cond.Broadcast()
				b.mu.Unlock()
				resultCh <- struct {
					task    BlockTask
					comp    []uint8
					err     error
					qwait   time.Duration
					breason string
					bwait   time.Duration
				}{task: task, comp: component, err: err, breason: budgetReason, bwait: budgetWait}
			}
		}()
	}
	go func() {
		wg.Wait()
		close(resultCh)
	}()
	go func() {
		defer close(taskCh)
		for _, task := range tasks {
			start := time.Now()
			reason := "ready"
			select {
			case taskCh <- roundTask{task: task}:
			case <-ctx.Done():
				s.addRecord(QueueRecord{
					TaskID: task.ID, StrokeID: task.StrokeID, StrokeOrder: task.StrokeOrdinal,
					BlockID: task.BlockID, Attempt: task.Attempt, QueueReason: "canceled",
					QueueWaitMS: time.Since(start).Milliseconds(), Outcome: "canceled",
				})
				return
			default:
				reason = "queue_budget"
				select {
				case taskCh <- roundTask{task: task}:
				case <-ctx.Done():
					s.addRecord(QueueRecord{
						TaskID: task.ID, StrokeID: task.StrokeID, StrokeOrder: task.StrokeOrdinal,
						BlockID: task.BlockID, Attempt: task.Attempt, QueueReason: "canceled",
						QueueWaitMS: time.Since(start).Milliseconds(), Outcome: "canceled",
					})
					return
				}
			}
			s.addRecord(QueueRecord{
				TaskID: task.ID, StrokeID: task.StrokeID, StrokeOrder: task.StrokeOrdinal,
				BlockID: task.BlockID, Attempt: task.Attempt, QueueReason: reason,
				QueueWaitMS:  time.Since(start).Milliseconds(),
				BudgetReason: "assigned", Outcome: "queued",
			})
		}
	}()
	var out []TaskResult
	for item := range resultCh {
		record := QueueRecord{
			TaskID: item.task.ID, StrokeID: item.task.StrokeID,
			StrokeOrder: item.task.StrokeOrdinal, BlockID: item.task.BlockID,
			Attempt: item.task.Attempt, QueueReason: "ready",
			BudgetReason: item.breason, BudgetWaitMS: item.bwait.Milliseconds(),
			Outcome: "succeeded",
		}
		if item.err != nil {
			record.Outcome = "failed"
			record.Error = item.err.Error()
		}
		s.addRecord(record)
		out = append(out, TaskResult{Task: item.task, Component: item.comp, Err: item.err})
	}
	if err := ctx.Err(); err != nil {
		return out, &CodedError{Code: CodeCanceled, Message: err.Error(), Err: err}
	}
	// resultCh order is completion order. Reorder for deterministic downstream
	// processing; workers never decide composition order.
	byID := make(map[string]TaskResult, len(out))
	for _, result := range out {
		byID[result.Task.ID] = result
	}
	ordered := make([]TaskResult, len(tasks))
	for i, task := range tasks {
		res, ok := byID[task.ID]
		if !ok {
			return out, coded(CodeExecution, "task %s missing result", task.ID)
		}
		ordered[i] = res
	}
	return ordered, nil
}
