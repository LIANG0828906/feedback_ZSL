package engine

import (
	"context"
	"sort"
	"sync"
	"time"

	"audioeval/internal/model"
)

type WorkerBuffer struct {
	X []int16
	Y []int16
}

func (b *WorkerBuffer) Allocate(samples int) ([]int16, []int16) {
	if cap(b.X) < samples {
		b.X = make([]int16, samples)
	} else {
		b.X = b.X[:samples]
	}
	if cap(b.Y) < samples {
		b.Y = make([]int16, samples)
	} else {
		b.Y = b.Y[:samples]
	}
	return b.X, b.Y
}

type Task struct {
	Sequence        int
	Coord           model.BlockCoord
	RequiredSamples int
	Run             func(context.Context, *WorkerBuffer) (model.BlockResult, error)
}

type Scheduler struct {
	MaxWorkers             int
	PerThreadBufferSamples int
	QueueBudget            int
}

type SchedulerSummary struct {
	Committed   []int
	QueueEvents []model.QueueEvent
}

type taskResult struct {
	task   Task
	result model.BlockResult
	err    error
}

func (s Scheduler) Run(ctx context.Context, tasks []Task, commit func(model.BlockResult) error) (SchedulerSummary, error) {
	summary := SchedulerSummary{}
	if len(tasks) == 0 {
		return summary, nil
	}
	if s.MaxWorkers <= 0 {
		return summary, model.NewError(model.CodePara, "max_workers", -1, "maximum worker count must be positive")
	}
	if s.PerThreadBufferSamples <= 0 {
		return summary, model.NewError(model.CodePara, "per_thread_buffer_samples", -1, "per-thread sample buffer must be positive")
	}
	if s.QueueBudget < 0 {
		return summary, model.NewError(model.CodePara, "queue_budget", -1, "queue budget must not be negative")
	}
	sort.Slice(tasks, func(i, j int) bool { return tasks[i].Sequence < tasks[j].Sequence })
	for i, task := range tasks {
		if i > 0 && tasks[i-1].Sequence == task.Sequence {
			return summary, model.NewError(model.CodeState, "sequence", task.Coord.Frame, "duplicate task sequence").WithBlock(task.Coord)
		}
		if task.RequiredSamples > s.PerThreadBufferSamples {
			err := model.NewError(model.CodePara, "per_thread_buffer_samples", task.Coord.Frame,
				"block requires more samples than the per-thread buffer budget").WithBlock(task.Coord)
			return summary, err
		}
	}

	runCtx, cancel := context.WithCancel(ctx)
	defer cancel()
	jobs := make(chan Task, s.QueueBudget)
	results := make(chan taskResult, max(1, s.MaxWorkers*2))
	var workerWG sync.WaitGroup
	for worker := 0; worker < s.MaxWorkers; worker++ {
		workerWG.Add(1)
		go func() {
			defer workerWG.Done()
			buffer := &WorkerBuffer{
				X: make([]int16, 0, s.PerThreadBufferSamples),
				Y: make([]int16, 0, s.PerThreadBufferSamples),
			}
			for task := range jobs {
				if runCtx.Err() != nil {
					break
				}
				result, err := task.Run(runCtx, buffer)
				results <- taskResult{task: task, result: result, err: err}
			}
		}()
	}

	var queueMu sync.Mutex
	producerDone := make(chan struct{})
	go func() {
		defer close(producerDone)
		defer close(jobs)
		for _, task := range tasks {
			if runCtx.Err() != nil {
				return
			}
			start := time.Now()
			waited := false
			select {
			case jobs <- task:
			case <-runCtx.Done():
				return
			default:
				waited = true
				select {
				case jobs <- task:
				case <-runCtx.Done():
					return
				}
			}
			if waited {
				event := model.QueueEvent{
					Sequence:   task.Sequence,
					Coord:      task.Coord,
					Reason:     "queue_budget_full",
					WaitMillis: time.Since(start).Milliseconds(),
				}
				queueMu.Lock()
				summary.QueueEvents = append(summary.QueueEvents, event)
				queueMu.Unlock()
			}
		}
	}()
	go func() {
		<-producerDone
		workerWG.Wait()
		close(results)
	}()

	expected := tasks[0].Sequence
	pending := map[int]taskResult{}
	var firstErr error
	for completed := range results {
		pending[completed.task.Sequence] = completed
		if completed.err != nil && firstErr == nil {
			firstErr = completed.err
			cancel()
		}
		for {
			next, ok := pending[expected]
			if !ok {
				break
			}
			if firstErr != nil || runCtx.Err() != nil {
				delete(pending, expected)
				break
			}
			if err := commit(next.result); err != nil {
				if firstErr == nil {
					firstErr = err
				}
				cancel()
				delete(pending, expected)
				break
			}
			summary.Committed = append(summary.Committed, expected)
			delete(pending, expected)
			expected++
		}
	}
	<-producerDone
	queueMu.Lock()
	defer queueMu.Unlock()
	if firstErr != nil {
		return summary, firstErr
	}
	if err := ctx.Err(); err != nil {
		return summary, err
	}
	return summary, nil
}
