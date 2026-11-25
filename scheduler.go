package iconmask

import (
	"context"
	"fmt"
	"sync"
	"sync/atomic"
	"time"
)

type ParallelConfig struct {
	MaxWorkers        int `json:"maxWorkers"`
	PixelBufferBudget int `json:"pixelBufferBudget"`
	QueueBudget       int `json:"queueBudget"`
}

func DefaultParallelConfig() ParallelConfig {
	return ParallelConfig{
		MaxWorkers:        2,
		PixelBufferBudget: 4096,
		QueueBudget:       2,
	}
}

type WaitEvent struct {
	Reason   string        `json:"reason"`
	Block    Coord         `json:"block"`
	Duration time.Duration `json:"durationNanos"`
}

type SchedulerReport struct {
	MaxWorkers        int         `json:"maxWorkers"`
	PixelBufferBudget int         `json:"pixelBufferBudget"`
	QueueBudget       int         `json:"queueBudget"`
	PeakInFlight      int         `json:"peakInFlight"`
	WaitEvents        []WaitEvent `json:"waitEvents"`
}

type PixelBuffer struct {
	Data []byte
	Cap  int
}

type ParallelTask struct {
	Index      int
	Block      Coord
	Rect       Rect
	PixelCount int
}

type ParallelFunc func(ctx context.Context, task ParallelTask, buffer *PixelBuffer) error

type taskResult struct {
	index int
	err   error
}

type queuedTask struct {
	task ParallelTask
}

func RunParallel(ctx context.Context, tasks []ParallelTask, cfg ParallelConfig, fn ParallelFunc) (SchedulerReport, error) {
	report := SchedulerReport{
		MaxWorkers:        cfg.MaxWorkers,
		PixelBufferBudget: cfg.PixelBufferBudget,
		QueueBudget:       cfg.QueueBudget,
	}
	if cfg.MaxWorkers <= 0 {
		return report, parameterError("<parallel>", "maxWorkers", "max workers must be a positive integer", "positive integer")
	}
	if cfg.PixelBufferBudget <= 0 {
		return report, parameterError("<parallel>", "pixelBufferBudget", "pixel buffer budget must be a positive integer", "positive integer")
	}
	if cfg.QueueBudget < 0 {
		return report, parameterError("<parallel>", "queueBudget", "queue budget must be non-negative", "non-negative integer")
	}
	if len(tasks) == 0 {
		return report, nil
	}
	jobs := make(chan queuedTask, cfg.QueueBudget)
	results := make(chan taskResult, len(tasks))
	pixelTokens := make(chan struct{}, cfg.MaxWorkers*cfg.PixelBufferBudget)
	var workers sync.WaitGroup
	var waitsMu sync.Mutex
	var inFlight int64
	var peakInFlight int64
	for worker := 0; worker < cfg.MaxWorkers; worker++ {
		workers.Add(1)
		go func() {
			defer workers.Done()
			for queued := range jobs {
				current := atomic.AddInt64(&inFlight, 1)
				for {
					peak := atomic.LoadInt64(&peakInFlight)
					if current <= peak || atomic.CompareAndSwapInt64(&peakInFlight, peak, current) {
						break
					}
				}
				if queued.task.PixelCount > cfg.PixelBufferBudget {
					results <- taskResult{
						index: queued.task.Index,
						err: parameterError("<parallel>", "pixelBufferBudget",
							fmt.Sprintf("task needs %d pixels but per-worker budget is %d",
								queued.task.PixelCount, cfg.PixelBufferBudget),
							"pixel buffer budget >= task pixel count"),
					}
					atomic.AddInt64(&inFlight, -1)
					continue
				}
				var blockedAt time.Time
				acquired := 0
				acquireCanceled := false
				for acquired < queued.task.PixelCount {
					select {
					case pixelTokens <- struct{}{}:
						acquired++
						continue
					default:
					}
					if blockedAt.IsZero() {
						blockedAt = time.Now()
					}
					select {
					case pixelTokens <- struct{}{}:
						acquired++
					case <-ctx.Done():
						acquireCanceled = true
					}
					if acquireCanceled {
						break
					}
				}
				if !blockedAt.IsZero() {
					waitsMu.Lock()
					report.WaitEvents = append(report.WaitEvents, WaitEvent{
						Reason:   "pixel_buffer_budget",
						Block:    queued.task.Block,
						Duration: time.Since(blockedAt),
					})
					waitsMu.Unlock()
				}
				if acquireCanceled {
					for i := 0; i < acquired; i++ {
						<-pixelTokens
					}
					results <- taskResult{index: queued.task.Index, err: ctx.Err()}
					atomic.AddInt64(&inFlight, -1)
					continue
				}
				buffer := &PixelBuffer{Data: make([]byte, queued.task.PixelCount), Cap: cfg.PixelBufferBudget}
				err := fn(ctx, queued.task, buffer)
				results <- taskResult{index: queued.task.Index, err: err}
				for i := 0; i < acquired; i++ {
					<-pixelTokens
				}
				atomic.AddInt64(&inFlight, -1)
			}
		}()
	}
	submitErr := error(nil)
	for _, task := range tasks {
		select {
		case jobs <- queuedTask{task: task}:
		case <-ctx.Done():
			submitErr = ctx.Err()
			break
		default:
			start := time.Now()
			select {
			case jobs <- queuedTask{task: task}:
				waitsMu.Lock()
				report.WaitEvents = append(report.WaitEvents, WaitEvent{
					Reason:   "queue_budget",
					Block:    task.Block,
					Duration: time.Since(start),
				})
				waitsMu.Unlock()
			case <-ctx.Done():
				submitErr = ctx.Err()
			}
		}
		if submitErr != nil {
			break
		}
	}
	close(jobs)
	workers.Wait()
	close(results)
	report.PeakInFlight = int(atomic.LoadInt64(&peakInFlight))
	var firstErr error
	for result := range results {
		if result.err != nil && firstErr == nil {
			firstErr = result.err
		}
	}
	if submitErr != nil && firstErr == nil {
		firstErr = submitErr
	}
	return report, firstErr
}
