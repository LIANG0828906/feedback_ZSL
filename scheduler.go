package ocrflow

import (
	"fmt"
	"sync"
	"time"
)

type BudgetConfig struct {
	Workers             int   `json:"workers"`
	PerWorkerPixelBytes int   `json:"per_worker_pixel_bytes"`
	MaxQueuedTasks      int   `json:"max_queued_tasks"`
	MaxQueuedPixelBytes int64 `json:"max_queued_pixel_bytes"`
}

func DefaultBudgetConfig() BudgetConfig {
	return BudgetConfig{
		Workers:             3,
		PerWorkerPixelBytes: 8 * 1024 * 1024,
		MaxQueuedTasks:      32,
		MaxQueuedPixelBytes: 64 * 1024 * 1024,
	}
}

type QueueWait struct {
	TaskID     string `json:"task_id"`
	Kind       string `json:"kind"`
	Reason     string `json:"reason"`
	WaitedMS   int64  `json:"waited_ms"`
	OccurredAt int64  `json:"occurred_at_unix_ns"`
}

type BudgetSnapshot struct {
	Config           BudgetConfig `json:"config"`
	ActiveWorkers    int          `json:"active_workers"`
	ActivePixelBytes int64        `json:"active_pixel_bytes"`
	QueuedTasks      int          `json:"queued_tasks"`
	QueuedPixelBytes int64        `json:"queued_pixel_bytes"`
	WaitCount        int64        `json:"wait_count"`
	TotalWaitMS      int64        `json:"total_wait_ms"`
	Waits            []QueueWait  `json:"recent_waits"`
}

type Task struct {
	ID         string
	Kind       string
	ShotID     string
	LineNo     int
	PixelBytes int
	Run        func() error
}

type TaskHandle struct {
	Task Task
	done chan error
}

func (h *TaskHandle) Done() <-chan error { return h.done }

type Scheduler struct {
	cfg  BudgetConfig
	mu   sync.Mutex
	cond *sync.Cond

	activeWorkers int
	activePixels  int64
	queuedTasks   int
	queuedPixels  int64
	waitCount     int64
	totalWaitMS   int64
	waits         []QueueWait
	workerSlots   chan struct{}
	wg            sync.WaitGroup
}

func NewScheduler(cfg BudgetConfig) (*Scheduler, error) {
	if cfg.Workers <= 0 {
		cfg.Workers = 1
	}
	if cfg.PerWorkerPixelBytes <= 0 {
		cfg.PerWorkerPixelBytes = 4 * 1024 * 1024
	}
	if cfg.MaxQueuedTasks <= 0 {
		cfg.MaxQueuedTasks = cfg.Workers * 4
	}
	if cfg.MaxQueuedPixelBytes <= 0 {
		cfg.MaxQueuedPixelBytes = int64(cfg.PerWorkerPixelBytes) * int64(cfg.MaxQueuedTasks)
	}
	s := &Scheduler{cfg: cfg, workerSlots: make(chan struct{}, cfg.Workers)}
	s.cond = sync.NewCond(&s.mu)
	return s, nil
}

func (s *Scheduler) Config() BudgetConfig { return s.cfg }

// Submit applies queue budgets before running a task. When a budget is
// exhausted it blocks, records the reason/wait, and never drops the task.
func (s *Scheduler) Submit(task Task) (*TaskHandle, error) {
	if task.Run == nil {
		return nil, fmt.Errorf("task %s has no function", task.ID)
	}
	if task.PixelBytes < 0 {
		return nil, paramError("task.pixel_bytes", []int{0, s.cfg.PerWorkerPixelBytes}, "pixel budget must be non-negative")
	}
	if task.PixelBytes > s.cfg.PerWorkerPixelBytes {
		return nil, &Error{Code: CodeResource, Field: "task.pixel_bytes", Message: fmt.Sprintf("task buffer %d exceeds per-worker cap %d", task.PixelBytes, s.cfg.PerWorkerPixelBytes), Meta: map[string]any{"task_id": task.ID}}
	}
	if int64(task.PixelBytes) > s.cfg.MaxQueuedPixelBytes {
		return nil, &Error{Code: CodeResource, Field: "budget.max_queued_pixel_bytes", Message: "task pixel footprint exceeds total queue budget", OperandA: fmt.Sprint(task.PixelBytes), OperandB: fmt.Sprint(s.cfg.MaxQueuedPixelBytes)}
	}

	handle := &TaskHandle{Task: task, done: make(chan error, 1)}
	s.mu.Lock()
	var waitReason string
	var waitStart time.Time
	for {
		reason := ""
		if s.queuedTasks >= s.cfg.MaxQueuedTasks {
			reason = "max_queued_tasks"
		} else if s.queuedPixels+int64(task.PixelBytes) > s.cfg.MaxQueuedPixelBytes {
			reason = "max_queued_pixel_bytes"
		}
		if reason == "" {
			break
		}
		if waitReason == "" {
			waitReason = reason
			waitStart = time.Now()
		}
		s.cond.Wait()
	}
	s.queuedTasks++
	s.queuedPixels += int64(task.PixelBytes)
	s.mu.Unlock()

	s.wg.Add(1)
	go func() {
		defer s.wg.Done()
		s.workerSlots <- struct{}{}

		s.mu.Lock()
		s.queuedTasks--
		s.queuedPixels -= int64(task.PixelBytes)
		s.activeWorkers++
		s.activePixels += int64(task.PixelBytes)
		if waitReason != "" {
			waited := time.Since(waitStart).Milliseconds()
			s.waitCount++
			s.totalWaitMS += waited
			w := QueueWait{TaskID: task.ID, Kind: task.Kind, Reason: waitReason, WaitedMS: waited, OccurredAt: time.Now().UnixNano()}
			s.waits = append(s.waits, w)
			if len(s.waits) > 50 {
				s.waits = append([]QueueWait(nil), s.waits[len(s.waits)-50:]...)
			}
		}
		s.cond.Broadcast()
		s.mu.Unlock()

		var err error
		func() {
			defer func() {
				if rec := recover(); rec != nil {
					err = &Error{Code: CodeResource, Field: "task.panic", Message: fmt.Sprintf("isolated task panic: %v", rec), Meta: map[string]any{"task_id": task.ID}}
				}
			}()
			err = task.Run()
		}()

		s.mu.Lock()
		s.activeWorkers--
		s.activePixels -= int64(task.PixelBytes)
		s.cond.Broadcast()
		s.mu.Unlock()
		<-s.workerSlots
		handle.done <- err
	}()
	return handle, nil
}

func (s *Scheduler) Snapshot() BudgetSnapshot {
	s.mu.Lock()
	defer s.mu.Unlock()
	waits := append([]QueueWait(nil), s.waits...)
	return BudgetSnapshot{
		Config:           s.cfg,
		ActiveWorkers:    s.activeWorkers,
		ActivePixelBytes: s.activePixels,
		QueuedTasks:      s.queuedTasks,
		QueuedPixelBytes: s.queuedPixels,
		WaitCount:        s.waitCount,
		TotalWaitMS:      s.totalWaitMS,
		Waits:            waits,
	}
}

// Wait blocks until all submitted task goroutines have finished.
func (s *Scheduler) Wait() { s.wg.Wait() }
