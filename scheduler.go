package mirror

import (
	"context"
	"fmt"
	"sync"
	"sync/atomic"
	"time"
)

// BudgetConfig bounds parallel mapping and packetization work. BufferPerWorker
// is the maximum prefetched tasks held by each worker; QueueBudget is the
// shared waiting queue. Both never silently drop work: producers block and the
// wait is recorded.
type BudgetConfig struct {
	MaxConcurrency  int `json:"maxConcurrency"`
	BufferPerWorker int `json:"bufferPerWorker"`
	QueueBudget     int `json:"queueBudget"`
}

func DefaultBudget() BudgetConfig {
	return BudgetConfig{MaxConcurrency: 4, BufferPerWorker: 2, QueueBudget: 8}
}

func (b BudgetConfig) Validate() error {
	if b.MaxConcurrency <= 0 {
		return errf(CodeState, "budget.maxConcurrency", "must be positive")
	}
	if b.BufferPerWorker <= 0 {
		return errf(CodeState, "budget.bufferPerWorker", "must be positive")
	}
	if b.QueueBudget < 0 {
		return errf(CodeState, "budget.queueBudget", "must be non-negative")
	}
	return nil
}

type QueueRecord struct {
	TaskID     string `json:"taskId"`
	Reason     string `json:"reason"`
	WaitMicros int64  `json:"waitMicros"`
}

type SchedulerStats struct {
	Config             BudgetConfig  `json:"config"`
	MaxObservedWorkers int           `json:"maxObservedWorkers"`
	MaxObservedQueue   int           `json:"maxObservedQueue"`
	QueueRecords       []QueueRecord `json:"queueRecords"`
}

type Task[T any] struct {
	ID  string
	Run func(context.Context) (T, error)
}

// RunTasks executes independent tasks in deterministic result-slot order.
// Worker and queue budgets bound resource use even though completion order may
// differ. Each task's errors are returned in its own slot for isolation.
func RunTasks[T any](ctx context.Context, tasks []Task[T], cfg BudgetConfig) ([]T, []error, *SchedulerStats, error) {
	if err := cfg.Validate(); err != nil {
		return nil, nil, nil, err
	}
	type submitted struct {
		index int
		task  Task[T]
	}
	results := make([]T, len(tasks))
	errs := make([]error, len(tasks))
	stats := &SchedulerStats{Config: cfg}
	if len(tasks) == 0 {
		return results, errs, stats, nil
	}
	queue := make(chan submitted, cfg.QueueBudget)
	inboxes := make([]chan submitted, cfg.MaxConcurrency)
	for i := range inboxes {
		inboxes[i] = make(chan submitted, cfg.BufferPerWorker)
	}
	done := make(chan struct{})
	dispatcherDone := make(chan struct{})
	var activeWorkers int64
	var maxWorkers int64
	var maxQueue int64
	var statMu sync.Mutex

	// Dispatcher block waits are useful evidence of per-worker buffer
	// pressure. It keeps round-robin assignment deterministic.
	go func() {
		defer close(dispatcherDone)
		next := 0
		for item := range queue {
			select {
			case inboxes[next] <- item:
			case <-ctx.Done():
				for i := next; i < len(inboxes); i++ {
					close(inboxes[i])
				}
				for i := 0; i < next; i++ {
					close(inboxes[i])
				}
				return
			}
			next = (next + 1) % len(inboxes)
		}
		for _, inbox := range inboxes {
			close(inbox)
		}
	}()

	var wg sync.WaitGroup
	for _, inbox := range inboxes {
		inbox := inbox
		wg.Add(1)
		go func() {
			defer wg.Done()
			for item := range inbox {
				count := atomic.AddInt64(&activeWorkers, 1)
				for {
					old := atomic.LoadInt64(&maxWorkers)
					if count <= old || atomic.CompareAndSwapInt64(&maxWorkers, old, count) {
						break
					}
				}
				results[item.index], errs[item.index] = safeRunTask(ctx, item.task)
				atomic.AddInt64(&activeWorkers, -1)
			}
		}()
	}

	go func() {
		wg.Wait()
		close(done)
	}()

	for i, task := range tasks {
		item := submitted{index: i, task: task}
		start := time.Now()
		select {
		case queue <- item:
		case <-ctx.Done():
			close(queue)
			<-dispatcherDone
			<-done
			return nil, nil, stats, &Error{Code: CodeState, Path: "budget.submit",
				Message: fmt.Sprintf("context cancelled while submitting %s", task.ID)}
		default:
			reason := "queue_full"
			select {
			case queue <- item:
			case <-ctx.Done():
				close(queue)
				<-dispatcherDone
				<-done
				return nil, nil, stats, &Error{Code: CodeState, Path: "budget.submit",
					Message: fmt.Sprintf("context cancelled while waiting for queue slot for %s", task.ID)}
			}
			wait := time.Since(start).Microseconds()
			statMu.Lock()
			stats.QueueRecords = append(stats.QueueRecords, QueueRecord{TaskID: task.ID, Reason: reason, WaitMicros: wait})
			statMu.Unlock()
		}
		depth := int64(len(queue))
		for {
			old := atomic.LoadInt64(&maxQueue)
			if depth <= old || atomic.CompareAndSwapInt64(&maxQueue, old, depth) {
				break
			}
		}
	}
	close(queue)
	<-dispatcherDone
	<-done
	stats.MaxObservedWorkers = int(atomic.LoadInt64(&maxWorkers))
	stats.MaxObservedQueue = int(atomic.LoadInt64(&maxQueue))
	if len(stats.QueueRecords) == 0 {
		stats.QueueRecords = []QueueRecord{}
	}
	return results, errs, stats, nil
}

func safeRunTask[T any](ctx context.Context, task Task[T]) (result T, err error) {
	defer func() {
		if r := recover(); r != nil {
			err = &Error{Code: CodeState, Path: task.ID, Message: fmt.Sprintf("task panic: %v", r)}
		}
	}()
	if err := ctx.Err(); err != nil {
		var zero T
		return zero, err
	}
	return task.Run(ctx)
}
