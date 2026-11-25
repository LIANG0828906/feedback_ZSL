package budget

import (
	"context"
	"fmt"
	"sort"
	"sync"
	"time"

	"terrainpipeline/internal/model"
)

type Options struct {
	MaxWorkers           int   `json:"max_workers"`
	PixelBufferPixels    int   `json:"pixel_buffer_pixels"`
	NormalMapMemoryBytes int64 `json:"normal_map_memory_bytes"`
}

func (o Options) WithDefaults() Options {
	if o.MaxWorkers <= 0 {
		o.MaxWorkers = 2
	}
	if o.PixelBufferPixels <= 0 {
		o.PixelBufferPixels = 64
	}
	if o.NormalMapMemoryBytes <= 0 {
		o.NormalMapMemoryBytes = 16 << 20
	}
	return o
}

type Task struct {
	ID          string
	PixelCount  int
	NormalBytes int64
	Run         func(scratch []model.PixelBake) error
}

type Event struct {
	Sequence      int       `json:"sequence"`
	At            time.Time `json:"at,omitempty"`
	BlockID       string    `json:"block_id"`
	Reason        string    `json:"reason"`
	Detail        string    `json:"detail,omitempty"`
	ActiveWorkers int       `json:"active_workers"`
	UsedBytes     int64     `json:"used_bytes"`
	WaitMS        int64     `json:"wait_ms,omitempty"`
}

type TaskError struct {
	BlockID string
	Err     error
}

func (e *TaskError) Error() string { return fmt.Sprintf("block %s: %v", e.BlockID, e.Err) }
func (e *TaskError) Unwrap() error { return e.Err }

type Scheduler struct {
	opts   Options
	mu     sync.Mutex
	cond   *sync.Cond
	used   int64
	active int
	events []Event
	seq    int
}

func NewScheduler(opts Options) *Scheduler {
	opts = opts.WithDefaults()
	s := &Scheduler{opts: opts}
	s.cond = sync.NewCond(&s.mu)
	return s
}

func (s *Scheduler) Options() Options { return s.opts }

func (s *Scheduler) Events() []Event {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]Event(nil), s.events...)
}

func (s *Scheduler) Run(ctx context.Context, tasks []Task) map[string]error {
	ordered := append([]Task(nil), tasks...)
	sort.SliceStable(ordered, func(i, j int) bool { return ordered[i].ID < ordered[j].ID })

	errs := map[string]error{}
	var errMu sync.Mutex
	var wg sync.WaitGroup
	workerSlots := make(chan struct{}, s.opts.MaxWorkers)

	for _, task := range ordered {
		if task.NormalBytes > s.opts.NormalMapMemoryBytes {
			errs[task.ID] = model.NewError(model.CodeBudget, task.ID, "normal_map_memory_bytes",
				"单块法线图需要 %d 字节，预算上限 %d", task.NormalBytes, s.opts.NormalMapMemoryBytes)
			continue
		}
		if err := ctx.Err(); err != nil {
			errs[task.ID] = err
			continue
		}
		s.acquireMemory(task)

		select {
		case workerSlots <- struct{}{}:
		default:
			s.record(task.ID, "max_workers", fmt.Sprintf("活跃 worker 已达 %d", s.opts.MaxWorkers), 0)
			workerSlots <- struct{}{}
		}

		wg.Add(1)
		go func(task Task) {
			defer wg.Done()
			defer func() { <-workerSlots }()
			defer s.releaseMemory(task.NormalBytes)

			s.mu.Lock()
			s.active++
			s.mu.Unlock()
			defer func() {
				s.mu.Lock()
				s.active--
				s.mu.Unlock()
			}()

			scratch := make([]model.PixelBake, min(s.opts.PixelBufferPixels, max(1, task.PixelCount)))
			if err := task.Run(scratch); err != nil {
				errMu.Lock()
				errs[task.ID] = &TaskError{BlockID: task.ID, Err: err}
				errMu.Unlock()
			}
		}(task)
	}
	wg.Wait()
	return errs
}

func (s *Scheduler) acquireMemory(task Task) {
	start := time.Now()
	queued := false
	s.mu.Lock()
	for s.used+task.NormalBytes > s.opts.NormalMapMemoryBytes {
		if !queued {
			s.recordLocked(task.ID, "normal_map_memory",
				fmt.Sprintf("需要 %d，当前占用 %d，预算 %d", task.NormalBytes, s.used, s.opts.NormalMapMemoryBytes), 0)
			queued = true
		}
		s.cond.Wait()
	}
	s.used += task.NormalBytes
	waitMS := time.Since(start).Milliseconds()
	if queued {
		s.recordLocked(task.ID, "normal_map_memory_acquired", "内存闸门已放行", waitMS)
	}
	s.mu.Unlock()
}

func (s *Scheduler) releaseMemory(bytes int64) {
	s.mu.Lock()
	s.used -= bytes
	if s.used < 0 {
		s.used = 0
	}
	s.cond.Broadcast()
	s.mu.Unlock()
}

func (s *Scheduler) record(blockID, reason, detail string, waitMS int64) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.recordLocked(blockID, reason, detail, waitMS)
}

func (s *Scheduler) recordLocked(blockID, reason, detail string, waitMS int64) {
	s.seq++
	s.events = append(s.events, Event{
		Sequence: s.seq, At: time.Now(), BlockID: blockID, Reason: reason, Detail: detail,
		ActiveWorkers: s.active, UsedBytes: s.used, WaitMS: waitMS,
	})
}

func min(a, b int) int {
	if a < b {
		return a
	}
	return b
}

func max(a, b int) int {
	if a > b {
		return a
	}
	return b
}
