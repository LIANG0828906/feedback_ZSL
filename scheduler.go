package loudness

import (
	"context"
	"fmt"
	"sync"
	"sync/atomic"
	"time"
)

// SchedulerConfig declares hard parallelism and memory/queue budgets.
type SchedulerConfig struct {
	MaxWorkers          int   `json:"maxWorkers"`
	QueueCapacity       int   `json:"queueCapacity"`
	QueueBudgetSamples  int64 `json:"queueBudgetSamples"`
	BufferBudgetSamples int64 `json:"bufferBudgetSamples"`
	MaxSamplesPerWorker int64 `json:"maxSamplesPerWorker"`
}

func DefaultSchedulerConfig() SchedulerConfig {
	return SchedulerConfig{
		MaxWorkers: 4, QueueCapacity: 2, QueueBudgetSamples: 1 << 20,
		BufferBudgetSamples: 1 << 20, MaxSamplesPerWorker: 1 << 20,
	}
}

type ScheduledTask struct {
	ID         int
	Label      string
	SampleCost int64
	Run        func(context.Context) (any, error)
}

// WaitEvent records why a task blocked and for how long.
type WaitEvent struct {
	TaskID     int    `json:"taskId"`
	Label      string `json:"label"`
	Reason     string `json:"reason"`
	WaitMicros int64  `json:"waitMicros"`
}

type SchedulerReport struct {
	Results            map[int]any `json:"-"`
	WaitEvents         []WaitEvent `json:"waitEvents"`
	MaxObservedWorkers int         `json:"maxObservedWorkers"`
}

func validateSchedulerConfig(config SchedulerConfig) error {
	if config.MaxWorkers < 1 || config.MaxWorkers > 256 {
		return &DomainError{Code: EPara, Path: "$.scheduler.maxWorkers", Field: "maxWorkers", LegalMin: 1, LegalMax: 256}
	}
	if config.QueueCapacity < 0 {
		return &DomainError{Code: EPara, Path: "$.scheduler.queueCapacity", Field: "queueCapacity", LegalMin: 0, LegalMax: 1 << 20}
	}
	if config.QueueBudgetSamples < 1 || config.BufferBudgetSamples < 1 || config.MaxSamplesPerWorker < 1 {
		return &DomainError{Code: EPara, Path: "$.scheduler", Field: "budgets", LegalMin: 1, LegalMax: 1 << 40}
	}
	if config.MaxSamplesPerWorker > config.BufferBudgetSamples {
		return &DomainError{Code: EPara, Path: "$.scheduler.maxSamplesPerWorker", Field: "maxSamplesPerWorker", LegalMin: 1, LegalMax: config.BufferBudgetSamples}
	}
	return nil
}

type semWaiter struct {
	cost    int64
	granted chan struct{}
}

type weightedSemaphore struct {
	mu      sync.Mutex
	current int64
	total   int64
	waiters []*semWaiter
}

func newWeightedSemaphore(total int64) *weightedSemaphore {
	return &weightedSemaphore{total: total}
}

func (s *weightedSemaphore) acquire(ctx context.Context, cost int64) (bool, error) {
	if cost < 0 || cost > s.total {
		return false, &DomainError{Code: EPara, Path: "$.scheduler", Field: "cost", LegalMin: 0, LegalMax: s.total}
	}
	s.mu.Lock()
	if s.current+cost <= s.total {
		s.current += cost
		s.mu.Unlock()
		return false, nil
	}
	waiter := &semWaiter{cost: cost, granted: make(chan struct{})}
	s.waiters = append(s.waiters, waiter)
	s.mu.Unlock()
	select {
	case <-waiter.granted:
		return true, nil
	case <-ctx.Done():
		s.mu.Lock()
		for i, candidate := range s.waiters {
			if candidate == waiter {
				s.waiters = append(s.waiters[:i], s.waiters[i+1:]...)
				break
			}
		}
		s.mu.Unlock()
		return true, ctx.Err()
	}
}

func (s *weightedSemaphore) release(cost int64) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.current -= cost
	if s.current < 0 {
		s.current = 0
	}
	remaining := s.waiters[:0]
	for _, waiter := range s.waiters {
		if s.current+waiter.cost <= s.total {
			s.current += waiter.cost
			close(waiter.granted)
			continue
		}
		remaining = append(remaining, waiter)
	}
	s.waiters = remaining
}

// RunScheduled runs tasks through a bounded worker pool. Sampling-buffer and
// queue budgets block rather than drop work; every block is reported.
func RunScheduled(ctx context.Context, config SchedulerConfig, tasks []ScheduledTask) (SchedulerReport, error) {
	if err := validateSchedulerConfig(config); err != nil {
		return SchedulerReport{}, err
	}
	report := SchedulerReport{Results: make(map[int]any), MaxObservedWorkers: 0}
	if len(tasks) == 0 {
		return report, nil
	}
	for _, task := range tasks {
		if task.SampleCost < 0 || task.SampleCost > config.MaxSamplesPerWorker {
			return report, &DomainError{
				Code: EPara, Path: fmt.Sprintf("$.tasks[%d].sampleCost", task.ID), Field: "sampleCost",
				Operands: []int64{task.SampleCost, config.MaxSamplesPerWorker}, LegalMin: 0, LegalMax: config.MaxSamplesPerWorker,
				Detail: "task exceeds per-worker sampling-buffer limit",
			}
		}
		if task.SampleCost > config.QueueBudgetSamples || task.SampleCost > config.BufferBudgetSamples {
			return report, &DomainError{
				Code: EPara, Path: fmt.Sprintf("$.tasks[%d].sampleCost", task.ID), Field: "sampleCost",
				Operands: []int64{task.SampleCost, config.BufferBudgetSamples}, LegalMin: 0, LegalMax: config.BufferBudgetSamples,
				Detail: "task cannot fit in a configured budget",
			}
		}
	}
	jobs := make(chan ScheduledTask, config.QueueCapacity)
	bufferSem := newWeightedSemaphore(config.BufferBudgetSamples)
	queueSem := newWeightedSemaphore(config.QueueBudgetSamples)
	var resultsMu sync.Mutex
	var waitsMu sync.Mutex
	var active atomic.Int64
	var maxActive atomic.Int64
	var firstError error
	var errorMu sync.Mutex
	var workers sync.WaitGroup
	recordWait := func(task ScheduledTask, reason string, start time.Time) {
		if start.IsZero() {
			return
		}
		elapsed := time.Since(start)
		if elapsed <= 0 {
			return
		}
		waitsMu.Lock()
		report.WaitEvents = append(report.WaitEvents, WaitEvent{
			TaskID: task.ID, Label: task.Label, Reason: reason, WaitMicros: elapsed.Microseconds(),
		})
		waitsMu.Unlock()
	}
	for workerID := 0; workerID < config.MaxWorkers; workerID++ {
		workers.Add(1)
		go func() {
			defer workers.Done()
			for task := range jobs {
				queueSem.release(task.SampleCost)
				if ctx.Err() != nil {
					continue
				}
				current := active.Add(1)
				for {
					observed := maxActive.Load()
					if current <= observed || maxActive.CompareAndSwap(observed, current) {
						break
					}
				}
				waitStart := time.Now()
				waited, err := bufferSem.acquire(ctx, task.SampleCost)
				if waited {
					recordWait(task, "sampling-buffer-budget", waitStart)
				}
				if err != nil {
					active.Add(-1)
					errorMu.Lock()
					if firstError == nil {
						firstError = err
					}
					errorMu.Unlock()
					continue
				}
				func() {
					defer func() {
						if recovered := recover(); recovered != nil {
							errorMu.Lock()
							if firstError == nil {
								firstError = fmt.Errorf("task %d panicked: %v", task.ID, recovered)
							}
							errorMu.Unlock()
						}
					}()
					value, runErr := task.Run(ctx)
					if runErr != nil {
						errorMu.Lock()
						if firstError == nil {
							firstError = runErr
						}
						errorMu.Unlock()
						return
					}
					resultsMu.Lock()
					report.Results[task.ID] = value
					resultsMu.Unlock()
				}()
				bufferSem.release(task.SampleCost)
				active.Add(-1)
			}
		}()
	}

	submitErr := func() error {
		defer close(jobs)
		for _, task := range tasks {
			if err := ctx.Err(); err != nil {
				return err
			}
			queueStart := time.Now()
			waited, err := queueSem.acquire(ctx, task.SampleCost)
			if waited {
				recordWait(task, "queue-sample-budget", queueStart)
			}
			if err != nil {
				return err
			}
			sendStart := time.Now()
			select {
			case jobs <- task:
			default:
				select {
				case jobs <- task:
					recordWait(task, "queue-capacity", sendStart)
				case <-ctx.Done():
					queueSem.release(task.SampleCost)
					return ctx.Err()
				}
			}
		}
		return nil
	}()
	workers.Wait()
	if submitErr != nil {
		return report, submitErr
	}
	if firstError != nil {
		return report, firstError
	}
	report.MaxObservedWorkers = int(maxActive.Load())
	return report, nil
}
