package scheduler

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"sync"
	"time"
)

type Config struct {
	MaxWorkers               int
	CandidateBufferPerWorker int
	PendingQueueBudget       int
}

func (c Config) normalized() Config {
	if c.MaxWorkers <= 0 {
		c.MaxWorkers = 1
	}
	if c.CandidateBufferPerWorker <= 0 {
		c.CandidateBufferPerWorker = 1
	}
	if c.PendingQueueBudget <= 0 {
		c.PendingQueueBudget = c.MaxWorkers
	}
	return c
}

type Task[T any] struct {
	ID     string
	Weight int
	Run    func(context.Context) (T, error)
}

type TaskResult[T any] struct {
	ID     string
	Value  T
	Err    error
	Worker int
	Wait   time.Duration
}

type WaitEvent struct {
	TaskID           string `json:"taskId"`
	Stage            string `json:"stage"`
	Reason           string `json:"reason"`
	WaitMilliseconds int64  `json:"waitMilliseconds"`
}

type Stats struct {
	MaxQueueDepth      int         `json:"maxQueueDepth"`
	MaxPendingParts    int         `json:"maxPendingParts"`
	PendingQueueBudget int         `json:"pendingQueueBudget"`
	MaxWorkers         int         `json:"maxWorkers"`
	PerWorkerBuffer    int         `json:"perWorkerBuffer"`
	TotalTaskWeight    int         `json:"totalTaskWeight"`
	WaitEvents         []WaitEvent `json:"waitEvents"`
}

type barrier struct {
	ctx     context.Context
	tasks   []Task[any]
	results []TaskResult[any]
	stats   Stats
}

// Run executes tasks with a fixed worker bound. The pending queue is bounded by
// PendingQueueBudget; enqueueing blocks and records a wait event when full.
// Each worker fills a local candidate buffer up to CandidateBufferPerWorker.
// A task error is isolated in its result and never stops other tasks.
func Run[T any](ctx context.Context, config Config, tasks []Task[T]) ([]TaskResult[T], Stats, error) {
	config = config.normalized()
	stats := Stats{
		PendingQueueBudget: config.PendingQueueBudget,
		MaxWorkers:         config.MaxWorkers,
		PerWorkerBuffer:    config.CandidateBufferPerWorker,
	}
	for _, task := range tasks {
		weight := task.Weight
		if weight <= 0 {
			weight = 1
		}
		stats.TotalTaskWeight += weight
	}
	if len(tasks) == 0 {
		return []TaskResult[T]{}, stats, nil
	}

	anyTasks := make([]Task[any], len(tasks))
	for index, task := range tasks {
		run := task.Run
		anyTasks[index] = Task[any]{
			ID:     task.ID,
			Weight: task.Weight,
			Run: func(ctx context.Context) (any, error) {
				return run(ctx)
			},
		}
	}
	b := &barrier{
		ctx:     ctx,
		tasks:   anyTasks,
		results: make([]TaskResult[any], len(anyTasks)),
		stats:   stats,
	}
	if err := b.run(); err != nil {
		return nil, b.stats, err
	}
	typed := make([]TaskResult[T], len(tasks))
	for index := range tasks {
		value, _ := b.results[index].Value.(T)
		typed[index] = TaskResult[T]{
			ID:     b.results[index].ID,
			Value:  value,
			Err:    b.results[index].Err,
			Worker: b.results[index].Worker,
			Wait:   b.results[index].Wait,
		}
	}
	sort.Slice(b.stats.WaitEvents, func(i, j int) bool {
		if b.stats.WaitEvents[i].TaskID != b.stats.WaitEvents[j].TaskID {
			return b.stats.WaitEvents[i].TaskID < b.stats.WaitEvents[j].TaskID
		}
		return b.stats.WaitEvents[i].Stage < b.stats.WaitEvents[j].Stage
	})
	stats = b.stats
	return typed, stats, nil
}

func (b *barrier) run() error {
	type indexedTask struct {
		index int
		task  Task[any]
		wait  time.Duration
	}
	jobs := make(chan indexedTask, len(b.tasks))
	var waitMu sync.Mutex
	var maxQueueDepth int
	var maxPendingParts int
	var gateMu sync.Mutex
	gate := sync.NewCond(&gateMu)
	pendingParts := 0
	stopBroadcast := make(chan struct{})
	go func() {
		select {
		case <-b.ctx.Done():
			gateMu.Lock()
			gate.Broadcast()
			gateMu.Unlock()
		case <-stopBroadcast:
		}
	}()
	release := func(weight int) {
		if weight <= 0 {
			weight = 1
		}
		gateMu.Lock()
		pendingParts -= weight
		if pendingParts < 0 {
			pendingParts = 0
		}
		gate.Broadcast()
		gateMu.Unlock()
	}
	var workers sync.WaitGroup

	for workerID := 0; workerID < b.stats.MaxWorkers; workerID++ {
		workers.Add(1)
		go func(workerID int) {
			defer workers.Done()
			buffer := make([]indexedTask, 0, b.stats.PerWorkerBuffer)
			bufferWeight := 0
			process := func() {
				for _, job := range buffer {
					if b.ctx.Err() != nil {
						b.results[job.index] = TaskResult[any]{
							ID:     job.task.ID,
							Err:    b.ctx.Err(),
							Worker: workerID,
						}
						continue
					}
					value, err := runSafely(b.ctx, job.task)
					b.results[job.index] = TaskResult[any]{
						ID:     job.task.ID,
						Value:  value,
						Err:    err,
						Worker: workerID,
						Wait:   job.wait,
					}
				}
				buffer = buffer[:0]
				bufferWeight = 0
			}
			for {
				job, ok := <-jobs
				if !ok {
					process()
					return
				}
				weight := job.task.Weight
				if weight <= 0 {
					weight = 1
				}
				release(weight)
				if len(buffer) > 0 && bufferWeight+weight > b.stats.PerWorkerBuffer {
					process()
				}
				buffer = append(buffer, job)
				bufferWeight += weight
			fill:
				for len(buffer) < cap(buffer) {
					select {
					case job, ok := <-jobs:
						if !ok {
							break fill
						}
						weight := job.task.Weight
						if weight <= 0 {
							weight = 1
						}
						release(weight)
						if bufferWeight+weight > b.stats.PerWorkerBuffer && len(buffer) > 0 {
							process()
						}
						buffer = append(buffer, job)
						bufferWeight += weight
					default:
						goto process
					}
				}
			process:
				process()
			}
		}(workerID)
	}

	for index, task := range b.tasks {
		if b.ctx.Err() != nil {
			b.results[index] = TaskResult[any]{ID: task.ID, Err: b.ctx.Err()}
			continue
		}
		weight := task.Weight
		if weight <= 0 {
			weight = 1
		}
		start := time.Now()
		gateMu.Lock()
		for pendingParts > 0 && pendingParts+weight > b.stats.PendingQueueBudget && b.ctx.Err() == nil {
			gate.Wait()
		}
		pendingParts += weight
		if pendingParts > maxPendingParts {
			maxPendingParts = pendingParts
		}
		waited := time.Since(start)
		gateMu.Unlock()
		select {
		case jobs <- indexedTask{index: index, task: task, wait: waited}:
		case <-b.ctx.Done():
			b.results[index] = TaskResult[any]{ID: task.ID, Err: b.ctx.Err()}
			release(weight)
			continue
		}
		if waited > 50*time.Microsecond {
			waitMu.Lock()
			b.stats.WaitEvents = append(b.stats.WaitEvents, WaitEvent{
				TaskID:           task.ID,
				Stage:            "part_queue",
				Reason:           "pending_part_budget_exhausted",
				WaitMilliseconds: waited.Milliseconds(),
			})
			waitMu.Unlock()
		}
		waitMu.Lock()
		if len(jobs) > maxQueueDepth {
			maxQueueDepth = len(jobs)
		}
		waitMu.Unlock()
	}
	close(jobs)
	workers.Wait()
	close(stopBroadcast)
	b.stats.MaxQueueDepth = maxQueueDepth
	b.stats.MaxPendingParts = maxPendingParts
	for index := range b.results {
		if b.results[index].ID == "" {
			b.results[index].ID = b.tasks[index].ID
		}
		if b.results[index].Err == nil && b.ctx.Err() != nil {
			b.results[index].Err = b.ctx.Err()
		}
	}
	return nil
}

var ErrNoWorkers = errors.New("scheduler: no workers")

func runSafely(ctx context.Context, task Task[any]) (value any, err error) {
	defer func() {
		if recovered := recover(); recovered != nil {
			err = fmt.Errorf("task %s panicked: %v", task.ID, recovered)
		}
	}()
	return task.Run(ctx)
}

func ValidateConfig(config Config) error {
	if config.MaxWorkers <= 0 {
		return fmt.Errorf("%w: MaxWorkers must be > 0", ErrNoWorkers)
	}
	if config.CandidateBufferPerWorker <= 0 {
		return fmt.Errorf("scheduler: CandidateBufferPerWorker must be > 0")
	}
	if config.PendingQueueBudget <= 0 {
		return fmt.Errorf("scheduler: PendingQueueBudget must be > 0")
	}
	return nil
}
