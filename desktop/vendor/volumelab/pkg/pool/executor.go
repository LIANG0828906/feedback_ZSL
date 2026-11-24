package pool

import (
	"context"
	"sync"
	"sync/atomic"

	"volumelab/pkg/model"
)

type Task struct {
	ID     string
	Pixels int
	Run    func(context.Context) error
}

type Stats struct {
	Submitted   int64 `json:"submitted"`
	Completed   int64 `json:"completed"`
	PeakWorkers int64 `json:"peakWorkers"`
	MaxWorkers  int   `json:"maxWorkers"`
	MaxPixels   int   `json:"maxPixelsPerWorker"`
}

// Executor bounds parallelism and the temporary pixel buffer reserved by each
// running task. Submissions block while all worker slots are occupied; they are
// never silently discarded.
type Executor struct {
	maxWorkers int
	maxPixels  int
	sem        chan struct{}

	mu       sync.Mutex
	stats    Stats
	running  int64
	firstErr error
}

func New(maxWorkers, maxPixelsPerWorker int) (*Executor, error) {
	if maxWorkers <= 0 {
		return nil, &model.AppError{Code: "E_ARG", Message: "max workers must be positive", Field: "maxParallelism", Actual: maxWorkers}
	}
	if maxPixelsPerWorker <= 0 {
		return nil, &model.AppError{Code: "E_ARG", Message: "max pixel buffer must be positive", Field: "maxBufferPixels", Actual: maxPixelsPerWorker}
	}
	return &Executor{
		maxWorkers: maxWorkers,
		maxPixels:  maxPixelsPerWorker,
		sem:        make(chan struct{}, maxWorkers),
		stats:      Stats{MaxWorkers: maxWorkers, MaxPixels: maxPixelsPerWorker},
	}, nil
}

func (e *Executor) MaxPixelsPerTask() int {
	return e.maxPixels
}

func (e *Executor) MaxWorkers() int {
	return e.maxWorkers
}

func (e *Executor) Run(ctx context.Context, tasks []Task) error {
	if err := ctx.Err(); err != nil {
		return &model.AppError{Code: "E_CANCEL", Message: "operation canceled before scheduling", Details: map[string]any{"cause": err.Error()}}
	}
	e.mu.Lock()
	e.firstErr = nil
	e.mu.Unlock()
	runCtx, cancel := context.WithCancel(ctx)
	defer cancel()

	var wg sync.WaitGroup
	for _, task := range tasks {
		if task.Pixels < 0 {
			return &model.AppError{Code: "E_ARG", Message: "task pixel count cannot be negative", Field: "pixels", Actual: task.Pixels}
		}
		if task.Pixels > e.maxPixels {
			return &model.AppError{
				Code:    "E_BUF",
				Message: "task pixel buffer exceeds the per-worker limit; split the task before submission",
				Actual:  task.Pixels,
				Allowed: map[string]any{"maxPixelsPerWorker": e.maxPixels},
			}
		}
		select {
		case e.sem <- struct{}{}:
		case <-runCtx.Done():
			wg.Wait()
			return &model.AppError{Code: "E_CANCEL", Message: "operation canceled while waiting for a worker slot", Details: map[string]any{"cause": runCtx.Err().Error()}}
		}
		if err := runCtx.Err(); err != nil {
			<-e.sem
			wg.Wait()
			return &model.AppError{Code: "E_CANCEL", Message: "operation canceled before task start"}
		}
		current := atomic.AddInt64(&e.running, 1)
		e.observePeak(current)
		atomic.AddInt64(&e.stats.Submitted, 1)
		wg.Add(1)
		go func(t Task) {
			defer wg.Done()
			defer func() {
				atomic.AddInt64(&e.running, -1)
				<-e.sem
			}()
			if runCtx.Err() != nil {
				return
			}
			if err := t.Run(runCtx); err != nil {
				e.mu.Lock()
				if e.firstErr == nil {
					e.firstErr = err
				}
				e.mu.Unlock()
				cancel()
				return
			}
			atomic.AddInt64(&e.stats.Completed, 1)
		}(task)
	}
	wg.Wait()
	e.mu.Lock()
	firstErr := e.firstErr
	e.mu.Unlock()
	if firstErr != nil {
		return firstErr
	}
	if err := ctx.Err(); err != nil {
		return &model.AppError{Code: "E_CANCEL", Message: "operation canceled", Details: map[string]any{"cause": err.Error()}}
	}
	return nil
}

func (e *Executor) observePeak(current int64) {
	e.mu.Lock()
	defer e.mu.Unlock()
	if current > e.stats.PeakWorkers {
		e.stats.PeakWorkers = current
	}
}

func (e *Executor) Stats() Stats {
	e.mu.Lock()
	defer e.mu.Unlock()
	return e.stats
}

func SplitPixels(totalPixels, maxPixels int) [][2]int {
	if totalPixels <= 0 || maxPixels <= 0 {
		return nil
	}
	var chunks [][2]int
	for start := 0; start < totalPixels; start += maxPixels {
		end := start + maxPixels
		if end > totalPixels {
			end = totalPixels
		}
		chunks = append(chunks, [2]int{start, end})
	}
	return chunks
}
