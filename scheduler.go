package maskqa

import (
	"fmt"
	"sync"
	"time"
)

// SchedulerOptions bounds local chunk parallelism and memory queues.
type SchedulerOptions struct {
	Parallelism     int `json:"parallelism"`
	BufferPixelCap  int `json:"buffer_pixel_cap"`
	QueueTileBudget int `json:"queue_tile_budget"`
}

// BudgetMetrics reports blocking and resource occupation.
type BudgetMetrics struct {
	ParallelismLimit  int              `json:"parallelism_limit"`
	BufferPixelCap    int              `json:"buffer_pixel_cap"`
	QueueTileBudget   int              `json:"queue_tile_budget"`
	PeakActiveWorkers int              `json:"peak_active_workers"`
	PeakQueueDepth    int              `json:"peak_queue_depth"`
	QueueWaitCount    int64            `json:"queue_wait_count"`
	QueueWaitMillis   int64            `json:"queue_wait_millis"`
	BufferWaitCount   int64            `json:"buffer_wait_count"`
	BufferWaitMillis  int64            `json:"buffer_wait_millis"`
	Backpressure      map[string]int64 `json:"backpressure,omitempty"`
}

// Job is one bounded unit of raster/accumulation work.
type Job struct {
	ID     string
	Pixels int
	Run    func(buffer []byte) error
}

// JobResult pairs a job ID with an isolated error.
type JobResult struct {
	ID  string `json:"id"`
	Err error  `json:"-"`
}

// Scheduler is a deterministic-result, bounded-resource worker pool.
type Scheduler struct {
	opts    SchedulerOptions
	buffers chan []byte
	metrics BudgetMetrics
	mu      sync.Mutex
}

func DefaultSchedulerOptions() SchedulerOptions {
	return SchedulerOptions{Parallelism: 2, BufferPixelCap: 4096, QueueTileBudget: 4}
}

// NewScheduler creates buffers up to the maximum worker count.
func NewScheduler(opts SchedulerOptions) *Scheduler {
	if opts.Parallelism <= 0 {
		opts.Parallelism = 1
	}
	if opts.BufferPixelCap <= 0 {
		opts.BufferPixelCap = 4096
	}
	if opts.QueueTileBudget <= 0 {
		opts.QueueTileBudget = opts.Parallelism
	}
	buffers := make(chan []byte, opts.Parallelism)
	for i := 0; i < opts.Parallelism; i++ {
		buffers <- make([]byte, opts.BufferPixelCap)
	}
	return &Scheduler{
		opts:    opts,
		buffers: buffers,
		metrics: BudgetMetrics{
			ParallelismLimit: opts.Parallelism, BufferPixelCap: opts.BufferPixelCap,
			QueueTileBudget: opts.QueueTileBudget, Backpressure: map[string]int64{},
		},
	}
}

// Snapshot returns a copy of budget metrics.
func (s *Scheduler) Snapshot() BudgetMetrics {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := s.metrics
	if s.metrics.Backpressure != nil {
		out.Backpressure = make(map[string]int64, len(s.metrics.Backpressure))
		for k, v := range s.metrics.Backpressure {
			out.Backpressure[k] = v
		}
	}
	return out
}

func (s *Scheduler) recordBackpressure(reason string, waited time.Duration, queue bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.metrics.Backpressure[reason]++
	if queue {
		s.metrics.QueueWaitCount++
		s.metrics.QueueWaitMillis += waited.Milliseconds()
	} else {
		s.metrics.BufferWaitCount++
		s.metrics.BufferWaitMillis += waited.Milliseconds()
	}
}

func (s *Scheduler) setPeakQueue(depth int) {
	s.mu.Lock()
	if depth > s.metrics.PeakQueueDepth {
		s.metrics.PeakQueueDepth = depth
	}
	s.mu.Unlock()
}

func (s *Scheduler) setActive(count int) {
	s.mu.Lock()
	if count > s.metrics.PeakActiveWorkers {
		s.metrics.PeakActiveWorkers = count
	}
	s.mu.Unlock()
}

func (s *Scheduler) acquireBuffer(jobID string) ([]byte, error) {
	select {
	case buf := <-s.buffers:
		return buf, nil
	default:
		start := time.Now()
		s.recordBackpressure("pixel_buffer_budget_exhausted:"+jobID, 0, false)
		buf := <-s.buffers
		waited := time.Since(start)
		s.mu.Lock()
		s.metrics.BufferWaitMillis += waited.Milliseconds()
		s.mu.Unlock()
		return buf, nil
	}
}

func (s *Scheduler) releaseBuffer(buf []byte) {
	for i := range buf {
		buf[i] = 0
	}
	s.buffers <- buf
}

// Run executes jobs and isolates panics as per-job errors.
func (s *Scheduler) Run(jobs []Job) []JobResult {
	results := make([]JobResult, len(jobs))
	work := make(chan int, s.opts.QueueTileBudget)
	var wg sync.WaitGroup
	var activeMu sync.Mutex
	active := 0

	for worker := 0; worker < s.opts.Parallelism; worker++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for idx := range work {
				activeMu.Lock()
				active++
				current := active
				activeMu.Unlock()
				s.setActive(current)

				results[idx].ID = jobs[idx].ID
				func() {
					defer func() {
						if recovered := recover(); recovered != nil {
							results[idx].Err = &Error{
								Code: "E_WORKER_PANIC", Field: jobs[idx].ID,
								Message: fmt.Sprintf("isolated job panic: %v", recovered),
							}
						}
					}()
					buf, err := s.acquireBuffer(jobs[idx].ID)
					if err != nil {
						results[idx].Err = err
						return
					}
					defer s.releaseBuffer(buf)
					if jobs[idx].Pixels > cap(buf) {
						results[idx].Err = paraError("scheduler.buffer_pixel_cap", 0,
							int64(s.opts.BufferPixelCap), "job needs %d pixels", jobs[idx].Pixels)
						return
					}
					results[idx].Err = jobs[idx].Run(buf[:jobs[idx].Pixels])
				}()

				activeMu.Lock()
				active--
				activeMu.Unlock()
			}
		}()
	}

	for i := range jobs {
		if len(work) == cap(work) {
			start := time.Now()
			s.recordBackpressure("tile_queue_budget_exhausted", 0, true)
			work <- i
			waited := time.Since(start)
			s.mu.Lock()
			s.metrics.QueueWaitMillis += waited.Milliseconds()
			s.mu.Unlock()
		} else {
			work <- i
		}
		s.setPeakQueue(len(work))
	}
	close(work)
	wg.Wait()
	return results
}
