package vpf

import (
	"context"
	"runtime"
	"sort"
	"sync"
	"sync/atomic"
	"time"
)

type SchedulerOptions struct {
	MaxParallel       int `json:"max_parallel"`
	MaxBufferedPixels int `json:"max_buffered_pixels_per_thread"`
	QueueBudget       int `json:"pending_chunk_queue_budget"`
	Retries           int `json:"retries"`
}

func DefaultSchedulerOptions() SchedulerOptions {
	return SchedulerOptions{
		MaxParallel:       minInt(4, runtime.GOMAXPROCS(0)),
		MaxBufferedPixels: 2048,
		QueueBudget:       2,
		Retries:           1,
	}
}

type QueueWait struct {
	ChunkIndex int    `json:"chunk_index"`
	WaitNS     int64  `json:"wait_ns"`
	Reason     string `json:"reason"`
}

type AttemptRecord struct {
	ChunkIndex int    `json:"chunk_index"`
	Attempt    int    `json:"attempt"`
	WorkerID   int    `json:"worker_id"`
	Error      string `json:"error,omitempty"`
}

type SchedulerReport struct {
	Options      SchedulerOptions `json:"options"`
	QueueWaits   []QueueWait      `json:"queue_waits"`
	Attempts     []AttemptRecord  `json:"attempts"`
	PeakParallel int32            `json:"peak_parallel"`
	mu           sync.Mutex
}

type ScheduledChunk struct {
	Index    int
	Result   *ChunkResult
	Err      error
	Attempts int
	WorkerID int
}

type BoundedScheduler struct {
	Options SchedulerOptions
}

func NewBoundedScheduler(options SchedulerOptions) *BoundedScheduler {
	if options.MaxParallel <= 0 {
		options.MaxParallel = DefaultSchedulerOptions().MaxParallel
	}
	if options.MaxBufferedPixels <= 0 {
		options.MaxBufferedPixels = DefaultSchedulerOptions().MaxBufferedPixels
	}
	if options.QueueBudget <= 0 {
		options.QueueBudget = options.MaxParallel
	}
	if options.Retries < 0 {
		options.Retries = 0
	}
	return &BoundedScheduler{Options: options}
}

type renderFunc func(chunkIndex, workerID int, buffer *PixelBuffer, attempt int) (*ChunkResult, error)

// Run renders all indices. Rendering is parallel; returned chunks are indexed
// and therefore independent of completion order.
func (s *BoundedScheduler) Run(ctx context.Context, indices []int, render renderFunc) ([]ScheduledChunk, *SchedulerReport, error) {
	report := &SchedulerReport{Options: s.Options}
	jobs := make(chan int, s.Options.QueueBudget)
	results := make(chan ScheduledChunk, len(indices))

	var producerWG sync.WaitGroup
	producerWG.Add(1)
	go func() {
		defer producerWG.Done()
		for _, index := range indices {
			start := time.Now()
			select {
			case jobs <- index:
				if waited := time.Since(start); waited > 0 {
					// A zero/tiny wait is normal fast admission; record only actual blocking.
					if len(jobs) == cap(jobs) {
						report.mu.Lock()
						report.QueueWaits = append(report.QueueWaits, QueueWait{
							ChunkIndex: index, WaitNS: waited.Nanoseconds(),
							Reason: "pending_queue_budget_exhausted",
						})
						report.mu.Unlock()
					}
				}
			case <-ctx.Done():
				return
			}
		}
		close(jobs)
	}()

	var workerWG sync.WaitGroup
	var active int32
	for workerID := 0; workerID < s.Options.MaxParallel; workerID++ {
		workerWG.Add(1)
		go func(workerID int) {
			defer workerWG.Done()
			buffer := NewPixelBuffer(s.Options.MaxBufferedPixels)
			for index := range jobs {
				current := atomic.AddInt32(&active, 1)
				updatePeak(&report.PeakParallel, current)
				outcome := ScheduledChunk{Index: index, WorkerID: workerID}
				for attempt := 1; attempt <= s.Options.Retries+1; attempt++ {
					outcome.Attempts = attempt
					report.mu.Lock()
					report.Attempts = append(report.Attempts, AttemptRecord{ChunkIndex: index, Attempt: attempt, WorkerID: workerID})
					report.mu.Unlock()
					result, err := render(index, workerID, buffer, attempt)
					if err == nil {
						outcome.Result = result
						outcome.Err = nil
						break
					}
					outcome.Err = err
					report.mu.Lock()
					report.Attempts[len(report.Attempts)-1].Error = err.Error()
					report.mu.Unlock()
				}
				results <- outcome
				atomic.AddInt32(&active, -1)
			}
		}(workerID)
	}

	go func() {
		producerWG.Wait()
		workerWG.Wait()
		close(results)
	}()

	outcomes := make([]ScheduledChunk, 0, len(indices))
	for outcome := range results {
		outcomes = append(outcomes, outcome)
	}
	producerWG.Wait()
	if err := ctx.Err(); err != nil {
		return outcomes, report, err
	}
	sort.Slice(outcomes, func(i, j int) bool { return outcomes[i].Index < outcomes[j].Index })
	return outcomes, report, nil
}

func updatePeak(peak *int32, current int32) {
	for {
		old := atomic.LoadInt32(peak)
		if current <= old || atomic.CompareAndSwapInt32(peak, old, current) {
			return
		}
	}
}

// mu is unexported via embedding? Define a small mutex field on report.
