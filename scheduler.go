package maskqc

import (
	"fmt"
	"sync"
	"time"
)

type SchedulerOptions struct {
	Parallelism    int `json:"parallelism"`
	PixelBufferCap int `json:"pixel_buffer_cap"`
	QueueBudget    int `json:"queue_budget"`
}

func DefaultSchedulerOptions() SchedulerOptions {
	return SchedulerOptions{Parallelism: 2, PixelBufferCap: 4096, QueueBudget: 2}
}

type Task struct {
	Index      int
	ChunkID    int
	StrokeID   string
	PixelCount int
	Run        func() error
}

type QueueEvent struct {
	ChunkID           int    `json:"chunk_id"`
	StrokeID          string `json:"stroke_id,omitempty"`
	Reason            string `json:"reason"`
	QueueLengthBefore int    `json:"queue_length_before"`
	QueueBudget       int    `json:"queue_budget"`
	WaitNanoseconds   int64  `json:"wait_nanoseconds"`
}

type Scheduler struct {
	opts   SchedulerOptions
	mu     sync.Mutex
	events []QueueEvent
}

func NewScheduler(opts SchedulerOptions) *Scheduler {
	if opts.Parallelism <= 0 {
		opts.Parallelism = 1
	}
	if opts.PixelBufferCap <= 0 {
		opts.PixelBufferCap = 4096
	}
	if opts.QueueBudget <= 0 {
		opts.QueueBudget = opts.Parallelism
	}
	return &Scheduler{opts: opts}
}

func (s *Scheduler) Options() SchedulerOptions { return s.opts }

func (s *Scheduler) Events() []QueueEvent {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]QueueEvent(nil), s.events...)
}

func (s *Scheduler) record(ev QueueEvent) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.events = append(s.events, ev)
}

// Run executes tasks with a fixed worker pool and bounded pending queue. A full
// queue blocks the submitter and records wait time; tasks are never dropped.
func (s *Scheduler) Run(tasks []Task) []error {
	opts := s.opts
	jobs := make(chan Task, opts.QueueBudget)
	errs := make([]error, len(tasks))
	var wg sync.WaitGroup
	for w := 0; w < opts.Parallelism; w++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for task := range jobs {
				if task.PixelCount > opts.PixelBufferCap {
					errs[task.Index] = &BizError{
						Code: CodePara, Message: "chunk pixel buffer exceeds per-thread cap",
						Field: "pixel_buffer_cap", ChunkID: task.ChunkID, StrokeID: task.StrokeID,
						Min: 0, Max: int64(opts.PixelBufferCap),
						Got: fmt.Sprint(task.PixelCount),
					}
					continue
				}
				if task.Run != nil {
					errs[task.Index] = task.Run()
				}
			}
		}()
	}
	for _, task := range tasks {
		before := len(jobs)
		reason := "admitted"
		if before >= opts.QueueBudget {
			reason = "queue-budget-exhausted"
		}
		start := time.Now()
		jobs <- task
		wait := time.Since(start).Nanoseconds()
		s.record(QueueEvent{
			ChunkID: task.ChunkID, StrokeID: task.StrokeID, Reason: reason,
			QueueLengthBefore: before, QueueBudget: opts.QueueBudget,
			WaitNanoseconds: wait,
		})
	}
	close(jobs)
	wg.Wait()
	return errs
}
