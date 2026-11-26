package engine

import (
	"context"
	"fmt"
	"sync"
	"sync/atomic"
	"time"
)

type blockFailure struct {
	StrokeID string
	Err      error
}

func (f *blockFailure) Error() string {
	if f.StrokeID == "" {
		return f.Err.Error()
	}
	return fmt.Sprintf("stroke %s: %v", f.StrokeID, f.Err)
}

func (f *blockFailure) Unwrap() error {
	return f.Err
}

func runScheduler(
	ctx context.Context,
	jobs []blockJob,
	config Config,
	compute func(context.Context, blockJob) (*BlockResult, error),
) schedulerResult {
	result := schedulerResult{
		blocks:   map[int]*BlockResult{},
		failures: map[string]error{},
		budget: BudgetReport{
			MaxParallel:          config.MaxParallel,
			PixelBufferPerWorker: config.PixelBufferPerWorker,
			QueueCapacity:        config.QueueCapacity,
		},
	}
	if len(jobs) == 0 {
		return result
	}
	jobQueue := make(chan blockJob, config.QueueCapacity)
	outcomes := make(chan blockOutcome, len(jobs))
	events := make(chan QueueEvent, len(jobs)*3)
	pixelTokens := make(chan int, config.MaxParallel*config.PixelBufferPerWorker)
	var running int64
	var maxRunning int64
	var activePixels int64
	var maxPixels int64
	var workers sync.WaitGroup
	for workerIndex := 0; workerIndex < config.MaxParallel; workerIndex++ {
		workers.Add(1)
		go func() {
			defer workers.Done()
			for job := range jobQueue {
				if ctx.Err() != nil {
					outcomes <- blockOutcome{result: nil, err: ctx.Err()}
					continue
				}
				queueWait := time.Since(job.enqueued)
				select {
				case pixelTokens <- 1:
					releaseTokens(pixelTokens, 1)
				default:
					blockedAt := time.Now()
					select {
					case pixelTokens <- 1:
						releaseTokens(pixelTokens, 1)
						events <- QueueEvent{
							BlockIndex: job.index, Stage: "pixel", Reason: "pixel_buffer_budget",
							WaitMicros: time.Since(blockedAt).Microseconds(),
						}
					case <-ctx.Done():
						outcomes <- blockOutcome{err: ctx.Err()}
						continue
					}
				}
				acquireAreaTokens(ctx, pixelTokens, job.area)
				currentRunning := atomic.AddInt64(&running, 1)
				updateMax(&maxRunning, currentRunning)
				currentPixels := atomic.AddInt64(&activePixels, int64(job.area))
				updateMax(&maxPixels, currentPixels)
				jobResult, err := safeCompute(ctx, compute, job)
				releaseTokens(pixelTokens, job.area)
				atomic.AddInt64(&activePixels, -int64(job.area))
				atomic.AddInt64(&running, -1)
				if jobResult != nil {
					jobResult.QueueWaitMicros = queueWait.Microseconds()
				}
				outcomes <- blockOutcome{result: jobResult, err: err}
			}
		}()
	}
submit:
	for _, job := range jobs {
		select {
		case jobQueue <- job:
		default:
			blockedAt := time.Now()
			select {
			case jobQueue <- job:
				events <- QueueEvent{
					BlockIndex: job.index, Stage: "queue", Reason: "queue_budget",
					WaitMicros: time.Since(blockedAt).Microseconds(),
				}
			case <-ctx.Done():
				break submit
			}
		}
	}
	close(jobQueue)
	workers.Wait()
	close(outcomes)
	close(events)
	for event := range events {
		result.budget.Events = append(result.budget.Events, event)
	}
	for outcome := range outcomes {
		if outcome.err != nil {
			if failure, ok := outcome.err.(*blockFailure); ok {
				result.failures[failure.StrokeID] = failure.Err
			}
			continue
		}
		if outcome.result != nil {
			result.blocks[outcome.result.Index] = outcome.result
		}
	}
	result.budget.MaxObservedParallel = int(atomic.LoadInt64(&maxRunning))
	result.budget.MaxObservedPixels = int(atomic.LoadInt64(&maxPixels))
	return result
}

func acquireAreaTokens(ctx context.Context, tokens chan int, area int) {
	if area <= 1 {
		select {
		case tokens <- 1:
		case <-ctx.Done():
		}
		return
	}
	for acquired := 0; acquired < area; {
		chunk := min(area-acquired, cap(tokens))
		for sent := 0; sent < chunk; sent++ {
			select {
			case tokens <- 1:
			case <-ctx.Done():
				releaseTokens(tokens, acquired+sent)
				return
			}
		}
		acquired += chunk
	}
}

func releaseTokens(tokens chan int, count int) {
	for released := 0; released < count; released++ {
		select {
		case <-tokens:
		default:
			return
		}
	}
}

func safeCompute(ctx context.Context, compute func(context.Context, blockJob) (*BlockResult, error), job blockJob) (result *BlockResult, err error) {
	defer func() {
		if recovered := recover(); recovered != nil {
			err = &blockFailure{
				StrokeID: "",
				Err:      fmt.Errorf("block %d panic: %v", job.index, recovered),
			}
		}
	}()
	return compute(ctx, job)
}

func updateMax(target *int64, value int64) {
	for {
		current := atomic.LoadInt64(target)
		if value <= current || atomic.CompareAndSwapInt64(target, current, value) {
			return
		}
	}
}
