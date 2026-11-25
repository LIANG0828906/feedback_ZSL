package maskqc

import (
	"fmt"
	"runtime/debug"
	"sort"
	"sync"
	"time"
)

type Job struct {
	Index  int
	ID     string
	TileID string
	Pixels int
	Fn     func(buffer []byte) error
}

type JobResult struct {
	Index      int    `json:"index"`
	JobID      string `json:"jobId"`
	TileID     string `json:"tileId"`
	OK         bool   `json:"ok"`
	ErrorCode  string `json:"errorCode,omitempty"`
	Error      string `json:"error,omitempty"`
	DurationMS int64  `json:"durationMs"`
	Panic      bool   `json:"panic,omitempty"`
}

type QueueWait struct {
	JobID  string `json:"jobId"`
	TileID string `json:"tileId"`
	Reason string `json:"reason"`
	WaitMS int64  `json:"waitMs"`
}

type SchedulerMetrics struct {
	MaxParallel     int         `json:"maxParallel"`
	MaxBufferPixels int         `json:"maxBufferPixels"`
	QueueBudget     int         `json:"queueBudget"`
	Submitted       int         `json:"submitted"`
	Succeeded       int         `json:"succeeded"`
	Failed          int         `json:"failed"`
	Waits           []QueueWait `json:"waits"`
}

type Scheduler struct {
	MaxParallel     int
	MaxBufferPixels int
	QueueBudget     int
	Metrics         SchedulerMetrics
	mu              sync.Mutex
}

func NewScheduler(maxParallel, maxBufferPixels, queueBudget int) *Scheduler {
	if maxParallel <= 0 {
		maxParallel = 2
	}
	if maxBufferPixels <= 0 {
		maxBufferPixels = 4096
	}
	if queueBudget < 0 {
		queueBudget = 0
	}
	return &Scheduler{
		MaxParallel: maxParallel, MaxBufferPixels: maxBufferPixels, QueueBudget: queueBudget,
		Metrics: SchedulerMetrics{MaxParallel: maxParallel, MaxBufferPixels: maxBufferPixels, QueueBudget: queueBudget},
	}
}

func (s *Scheduler) Run(jobs []Job) []JobResult {
	if s.MaxParallel <= 0 {
		s.MaxParallel = 2
	}
	if s.MaxBufferPixels <= 0 {
		s.MaxBufferPixels = 4096
	}
	queue := make(chan Job, s.QueueBudget)
	results := make([]JobResult, len(jobs))
	var wg sync.WaitGroup

	for worker := 0; worker < s.MaxParallel; worker++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			buffer := make([]byte, s.MaxBufferPixels)
			for job := range queue {
				results[job.Index] = s.execute(job, buffer)
			}
		}()
	}

	for _, job := range jobs {
		if job.ID == "" {
			job.ID = fmt.Sprintf("job-%d", job.Index)
		}
		if job.Pixels > s.MaxBufferPixels {
			results[job.Index] = JobResult{
				Index: job.Index, JobID: job.ID, TileID: job.TileID, OK: false,
				ErrorCode: CodeBudget,
				Error:     fmt.Sprintf("job needs %d pixels but per-thread buffer limit is %d", job.Pixels, s.MaxBufferPixels),
			}
			s.recordFinish(false)
			continue
		}
		start := time.Now()
		select {
		case queue <- job:
		default:
			waitStart := time.Now()
			s.recordWait(QueueWait{JobID: job.ID, TileID: job.TileID, Reason: "queue_budget_exhausted"})
			queue <- job
			s.updateLastWait(time.Since(waitStart))
		}
		_ = start
		s.recordSubmitted()
	}
	close(queue)
	wg.Wait()

	for _, r := range results {
		if r.JobID == "" && r.Error == "" {
			// Index skipped by an early budget failure already has a result.
			continue
		}
		if r.OK {
			s.Metrics.Succeeded++
		} else if r.Error != "" {
			s.Metrics.Failed++
		}
	}
	s.Metrics.Submitted = len(jobs)
	sort.SliceStable(s.Metrics.Waits, func(i, j int) bool { return s.Metrics.Waits[i].JobID < s.Metrics.Waits[j].JobID })
	return results
}

func (s *Scheduler) execute(job Job, buffer []byte) (result JobResult) {
	start := time.Now()
	result = JobResult{Index: job.Index, JobID: job.ID, TileID: job.TileID}
	defer func() {
		if recovered := recover(); recovered != nil {
			result.OK = false
			result.Panic = true
			result.ErrorCode = CodeInternal
			result.Error = fmt.Sprintf("panic isolated: %v\n%s", recovered, debug.Stack())
		}
		result.DurationMS = time.Since(start).Milliseconds()
	}()
	if job.Fn == nil {
		result.OK = true
		return
	}
	if err := job.Fn(buffer); err != nil {
		if qerr, ok := err.(*QCError); ok {
			result.ErrorCode = qerr.Code
			result.Error = qerr.Error()
		} else {
			result.ErrorCode = CodeInternal
			result.Error = err.Error()
		}
		return result
	}
	result.OK = true
	return result
}

func (s *Scheduler) recordWait(w QueueWait) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.Metrics.Waits = append(s.Metrics.Waits, w)
}

func (s *Scheduler) updateLastWait(d time.Duration) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if len(s.Metrics.Waits) > 0 {
		s.Metrics.Waits[len(s.Metrics.Waits)-1].WaitMS = d.Milliseconds()
	}
}

func (s *Scheduler) recordSubmitted() {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.Metrics.Submitted++
}

func (s *Scheduler) recordFinish(_ bool) {
	// Final counters are reconciled after Run; this hook keeps early rejection
	// accounting explicit and safe if Run is later refactored to stream.
}
