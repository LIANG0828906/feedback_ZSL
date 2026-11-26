package maskqc

import (
	"context"
	"fmt"
	"sync"
	"time"
)

type BudgetConfig struct {
	MaxParallel        int `json:"max_parallel"`
	MaxPixelsPerWorker int `json:"max_pixels_per_worker"`
	MaxQueuedTiles     int `json:"max_queued_tiles"`
}

func DefaultBudgetConfig() BudgetConfig {
	return BudgetConfig{MaxParallel: 2, MaxPixelsPerWorker: 4096, MaxQueuedTiles: 12}
}

type QueueWait struct {
	TileID   string `json:"tile_id"`
	Reason   string `json:"reason"`
	WaitedMS int64  `json:"waited_ms"`
}

type BudgetSnapshot struct {
	Config             BudgetConfig `json:"config"`
	ActiveWorkers      int          `json:"active_workers"`
	OutstandingTiles   int          `json:"outstanding_tiles"`
	PeakOutstanding    int          `json:"peak_outstanding"`
	ActiveBufferPixels int          `json:"active_buffer_pixels"`
	PeakBufferPixels   int          `json:"peak_buffer_pixels"`
	Processed          int          `json:"processed"`
	Failed             int          `json:"failed"`
	Waits              []QueueWait  `json:"waits"`
}

type TileJob struct {
	ID     string
	Pixels int
	Run    func(ctx context.Context) error
}

type JobResult struct {
	TileID string `json:"tile_id"`
	OK     bool   `json:"ok"`
	Error  error  `json:"-"`
	Code   string `json:"code,omitempty"`
}

type Scheduler struct {
	cfg BudgetConfig

	workerSlots chan struct{}
	queueSlots  chan struct{}

	mu          sync.Mutex
	active      int
	outstanding int
	peakQueue   int
	bufferPx    int
	peakBuffer  int
	processed   int
	failed      int
	waits       []QueueWait
}

func NewScheduler(cfg BudgetConfig) *Scheduler {
	if cfg.MaxParallel <= 0 {
		cfg.MaxParallel = 1
	}
	if cfg.MaxPixelsPerWorker <= 0 {
		cfg.MaxPixelsPerWorker = 64 * 64
	}
	if cfg.MaxQueuedTiles <= 0 {
		cfg.MaxQueuedTiles = cfg.MaxParallel
	}
	return &Scheduler{
		cfg:         cfg,
		workerSlots: make(chan struct{}, cfg.MaxParallel),
		queueSlots:  make(chan struct{}, cfg.MaxQueuedTiles),
	}
}

func (s *Scheduler) Config() BudgetConfig { return s.cfg }

func (s *Scheduler) Snapshot() BudgetSnapshot {
	s.mu.Lock()
	defer s.mu.Unlock()
	waits := append([]QueueWait(nil), s.waits...)
	if len(waits) > 50 {
		waits = waits[len(waits)-50:]
	}
	return BudgetSnapshot{
		Config: s.cfg, ActiveWorkers: s.active, OutstandingTiles: s.outstanding,
		PeakOutstanding: s.peakQueue, ActiveBufferPixels: s.bufferPx, PeakBufferPixels: s.peakBuffer,
		Processed: s.processed, Failed: s.failed, Waits: waits,
	}
}

func (s *Scheduler) recordWait(w QueueWait) {
	s.mu.Lock()
	s.waits = append(s.waits, w)
	s.mu.Unlock()
}

// Run executes all jobs and always returns one result per submitted job. A
// failed job does not cancel siblings. Queue saturation blocks the caller and
// records the wait instead of dropping work.
func (s *Scheduler) Run(ctx context.Context, jobs []TileJob) []JobResult {
	results := make([]JobResult, len(jobs))
	var wg sync.WaitGroup

	for i, job := range jobs {
		if job.Pixels > s.cfg.MaxPixelsPerWorker {
			results[i] = JobResult{TileID: job.ID, OK: false, Code: string(EBudget), Error: &Error{
				Code: EBudget, FieldPath: job.ID,
				Message: fmt.Sprintf("tile buffer %d pixels exceeds per-worker cap %d", job.Pixels, s.cfg.MaxPixelsPerWorker),
				Details: map[string]any{"pixels": job.Pixels, "cap": s.cfg.MaxPixelsPerWorker},
			}}
			s.mu.Lock()
			s.failed++
			s.mu.Unlock()
			continue
		}

		queueStart := time.Now()
		queueWasFull := len(s.queueSlots) >= cap(s.queueSlots)
		select {
		case s.queueSlots <- struct{}{}:
		case <-ctx.Done():
			results[i] = JobResult{TileID: job.ID, OK: false, Code: "E_CANCELLED", Error: ctx.Err()}
			return results
		}
		if queueWasFull {
			s.recordWait(QueueWait{TileID: job.ID, Reason: "queue_budget_exhausted", WaitedMS: time.Since(queueStart).Milliseconds()})
		}
		s.mu.Lock()
		s.outstanding++
		if s.outstanding > s.peakQueue {
			s.peakQueue = s.outstanding
		}
		s.mu.Unlock()

		workerStart := time.Now()
		workersFull := len(s.workerSlots) >= cap(s.workerSlots)
		select {
		case s.workerSlots <- struct{}{}:
		case <-ctx.Done():
			<-s.queueSlots
			s.decrementOutstanding()
			results[i] = JobResult{TileID: job.ID, OK: false, Code: "E_CANCELLED", Error: ctx.Err()}
			return results
		}
		if workersFull {
			waited := time.Since(workerStart).Milliseconds()
			if waited > 0 {
				s.recordWait(QueueWait{TileID: job.ID, Reason: "max_parallel_slots_exhausted", WaitedMS: waited})
			}
		}

		wg.Add(1)
		go func(i int, job TileJob) {
			defer wg.Done()
			defer func() {
				<-s.workerSlots
				<-s.queueSlots
				s.decrementOutstanding()
				s.mu.Lock()
				s.active--
				s.bufferPx -= job.Pixels
				if results[i].OK {
					s.processed++
				} else {
					s.failed++
				}
				s.mu.Unlock()
				if r := recover(); r != nil {
					de := newError(EBudget, job.ID, "tile worker panic isolated: %v", r)
					results[i] = JobResult{TileID: job.ID, OK: false, Code: string(de.Code), Error: de}
				}
			}()
			s.mu.Lock()
			s.active++
			s.bufferPx += job.Pixels
			if s.bufferPx > s.peakBuffer {
				s.peakBuffer = s.bufferPx
			}
			s.mu.Unlock()

			err := job.Run(ctx)
			if err != nil {
				code := "E_TILE"
				if de, ok := AsError(err); ok {
					code = string(de.Code)
				}
				results[i] = JobResult{TileID: job.ID, OK: false, Code: code, Error: err}
			} else {
				results[i] = JobResult{TileID: job.ID, OK: true}
			}
		}(i, job)
	}
	wg.Wait()
	return results
}

func (s *Scheduler) decrementOutstanding() {
	s.mu.Lock()
	s.outstanding--
	s.mu.Unlock()
}
