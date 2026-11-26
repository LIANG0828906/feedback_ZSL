package maskqa

import (
	"context"
	"fmt"
	"sync"
	"time"
)

type WaitRecord struct {
	BlockID string        `json:"block_id"`
	Stage   Stage         `json:"stage"`
	Reason  string        `json:"reason"`
	Waited  time.Duration `json:"waited"`
	Pixels  int           `json:"pixels"`
}

type Job struct {
	ID     string
	Block  BlockCoord
	Stage  Stage
	Pixels int
	Run    func(buf []byte) error
}

type JobResult struct {
	Job    Job
	Err    error
	Waited time.Duration
}

type SchedulerMetrics struct {
	MaxParallel  int          `json:"max_parallel"`
	BufferLimit  int          `json:"buffer_limit_pixels"`
	QueueBudget  int          `json:"queue_pixel_budget"`
	PeakParallel int          `json:"peak_parallel"`
	Submitted    int          `json:"submitted"`
	Completed    int          `json:"completed"`
	Failed       int          `json:"failed"`
	WaitRecords  []WaitRecord `json:"wait_records"`
}

type BoundedScheduler struct {
	cfg     Params
	metrics SchedulerMetrics
	mu      sync.Mutex
	cond    *sync.Cond
	used    int
	active  int
}

func NewBoundedScheduler(p Params) *BoundedScheduler {
	s := &BoundedScheduler{cfg: p}
	s.metrics = SchedulerMetrics{MaxParallel: p.MaxParallelBlocks, BufferLimit: p.ThreadPixelBufferLimit, QueueBudget: p.QueuedPixelBudget}
	s.cond = sync.NewCond(&s.mu)
	return s
}

func (s *BoundedScheduler) Metrics() SchedulerMetrics {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := s.metrics
	out.WaitRecords = append([]WaitRecord(nil), s.metrics.WaitRecords...)
	return out
}

func (s *BoundedScheduler) Run(ctx context.Context, jobs []Job) []JobResult {
	results := make([]JobResult, len(jobs))
	work := make(chan int)
	var wg sync.WaitGroup
	for w := 0; w < s.cfg.MaxParallelBlocks; w++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			buf := make([]byte, s.cfg.ThreadPixelBufferLimit)
			for idx := range work {
				j := jobs[idx]
				s.mu.Lock()
				s.active++
				if s.active > s.metrics.PeakParallel {
					s.metrics.PeakParallel = s.active
				}
				s.mu.Unlock()
				err := j.Run(buf)
				s.mu.Lock()
				s.active--
				s.used -= j.Pixels
				s.metrics.Completed++
				if err != nil {
					s.metrics.Failed++
				}
				s.cond.Broadcast()
				s.mu.Unlock()
				results[idx].Err = err
			}
		}()
	}

	for i, job := range jobs {
		if job.Pixels > s.cfg.ThreadPixelBufferLimit {
			results[i] = JobResult{Job: job, Err: paraError("thread_pixel_buffer_limit", job.Pixels, s.cfg.ThreadPixelBufferLimit, job.Pixels, "job pixel buffer exceeds per-thread limit")}
			continue
		}
		if job.Pixels > s.cfg.QueuedPixelBudget {
			err := paraError("queued_pixel_budget", job.Pixels, s.cfg.QueuedPixelBudget, job.Pixels, "single job exceeds total queue pixel budget")
			results[i] = JobResult{Job: job, Err: err}
			continue
		}
		s.mu.Lock()
		waitStart := time.Time{}
		exhausted := false
		for s.used+job.Pixels > s.cfg.QueuedPixelBudget {
			if !exhausted {
				waitStart = time.Now()
				exhausted = true
			}
			if ctx.Err() != nil {
				s.mu.Unlock()
				results[i] = JobResult{Job: job, Err: ctx.Err()}
				close(work)
				wg.Wait()
				return results
			}
			s.cond.Wait()
		}
		waited := time.Duration(0)
		if exhausted {
			waited = time.Since(waitStart)
		}
		s.used += job.Pixels
		s.metrics.Submitted++
		if exhausted {
			s.metrics.WaitRecords = append(s.metrics.WaitRecords, WaitRecord{BlockID: job.Block.ID(), Stage: job.Stage, Reason: "queue_pixel_budget_exhausted", Waited: waited, Pixels: job.Pixels})
		}
		s.mu.Unlock()
		results[i] = JobResult{Job: job, Waited: waited}
		select {
		case work <- i:
		case <-ctx.Done():
			results[i].Err = ctx.Err()
		}
	}
	close(work)
	wg.Wait()
	return results
}

func ValidateSchedulerBudget(p Params, block BlockCoord) error {
	pixels := block.Width * block.Height
	if pixels > p.ThreadPixelBufferLimit {
		return paraError("thread_pixel_buffer_limit", pixels, p.ThreadPixelBufferLimit, pixels, fmt.Sprintf("block %s exceeds thread buffer", block.ID()))
	}
	if pixels > p.QueuedPixelBudget {
		return paraError("queued_pixel_budget", pixels, p.QueuedPixelBudget, pixels, fmt.Sprintf("block %s exceeds queue budget", block.ID()))
	}
	return nil
}
