package workpool

import (
	"context"
	"fmt"
	"sync"
	"time"
)

type Event struct {
	Index  int           `json:"index"`
	ID     string        `json:"id"`
	Kind   string        `json:"kind"`
	Reason string        `json:"reason"`
	Wait   time.Duration `json:"wait_ns"`
}

type Pool struct {
	Workers         int
	PerWorkerBuffer int
	QueueBudget     int
}

type job struct {
	index int
	id    string
}

type taskResult struct {
	index int
	err   error
	panic any
}

// Run executes tasks with bounded worker parallelism, a bounded ingress queue
// and a bounded per-worker buffer. Submission blocks and emits a queue_wait
// event when capacity is exhausted; tasks are never silently dropped.
func (p Pool) Run(ctx context.Context, ids []string, fn func(context.Context, int, string) error) ([]error, []Event, error) {
	if p.Workers < 1 || p.PerWorkerBuffer < 0 || p.QueueBudget < 1 {
		return nil, nil, fmt.Errorf("invalid pool limits: workers=%d worker_buffer=%d queue_budget=%d",
			p.Workers, p.PerWorkerBuffer, p.QueueBudget)
	}
	results := make([]error, len(ids))
	submitted := make([]bool, len(ids))
	received := make([]bool, len(ids))
	if len(ids) == 0 {
		return results, nil, nil
	}
	jobs := make(chan job, p.QueueBudget)
	locals := make([]chan job, p.Workers)
	for i := range locals {
		locals[i] = make(chan job, p.PerWorkerBuffer)
	}
	resultCh := make(chan taskResult, len(ids))
	var eventMu sync.Mutex
	var events []Event
	appendEvent := func(event Event) {
		eventMu.Lock()
		events = append(events, event)
		eventMu.Unlock()
	}

	var workerWG sync.WaitGroup
	for i := range locals {
		workerWG.Add(1)
		go func(local <-chan job) {
			defer workerWG.Done()
			for j := range local {
				err, panicValue := invoke(ctx, fn, j)
				resultCh <- taskResult{index: j.index, err: err, panic: panicValue}
			}
		}(locals[i])
	}

	dispatcherDone := make(chan struct{})
	go func() {
		defer close(dispatcherDone)
		defer func() {
			for _, local := range locals {
				close(local)
			}
		}()
		next := 0
		for {
			select {
			case <-ctx.Done():
				return
			case j, ok := <-jobs:
				if !ok {
					return
				}
				dispatched := false
				for attempt := 0; attempt < len(locals); attempt++ {
					idx := (next + attempt) % len(locals)
					select {
					case locals[idx] <- j:
						next = (idx + 1) % len(locals)
						dispatched = true
					default:
					}
					if dispatched {
						break
					}
				}
				if dispatched {
					continue
				}
				waited := time.Now()
				select {
				case <-ctx.Done():
					continue
				case locals[next] <- j:
					next = (next + 1) % len(locals)
				}
				appendEvent(Event{
					Index: j.index, ID: j.id, Kind: "worker_wait",
					Reason: "all_worker_buffers_full", Wait: time.Since(waited),
				})
			}
		}
	}()

	cancelled := false
	for i, id := range ids {
		if ctx.Err() != nil {
			results[i] = ctx.Err()
			cancelled = true
			continue
		}
		select {
		case jobs <- job{index: i, id: id}:
			submitted[i] = true
		default:
			waited := time.Now()
			select {
			case jobs <- job{index: i, id: id}:
				submitted[i] = true
				appendEvent(Event{
					Index: i, ID: id, Kind: "queue_wait",
					Reason: "queue_budget_exhausted", Wait: time.Since(waited),
				})
			case <-ctx.Done():
				results[i] = ctx.Err()
				cancelled = true
			}
		}
	}
	close(jobs)
	<-dispatcherDone
	workerWG.Wait()
	close(resultCh)
	for result := range resultCh {
		received[result.index] = true
		if result.panic != nil {
			results[result.index] = fmt.Errorf("task panic: %v", result.panic)
			continue
		}
		results[result.index] = result.err
	}
	for i := range results {
		if submitted[i] && !received[i] {
			if err := ctx.Err(); err != nil {
				results[i] = err
			} else {
				results[i] = fmt.Errorf("task was not executed")
			}
		} else if results[i] == nil && cancelled && ctx.Err() != nil {
			results[i] = ctx.Err()
		}
	}
	return results, events, nil
}

func invoke(ctx context.Context, fn func(context.Context, int, string) error, j job) (err error, panicValue any) {
	defer func() {
		if p := recover(); p != nil {
			panicValue = p
		}
	}()
	return fn(ctx, j.index, j.id), nil
}
