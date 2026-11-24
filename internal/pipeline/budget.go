package pipeline

import (
	"context"
	"sync"
	"time"

	"maskpipe/internal/model"
)

const BufferUnitsPerPixel = 5

type QueueEvent struct {
	Time          string `json:"time"`
	Chunk         string `json:"chunk"`
	Stage         string `json:"stage"`
	Reason        string `json:"reason"`
	QueueBudget   int    `json:"queue_budget"`
	WaitingMillis int64  `json:"waiting_ms"`
}

type BudgetMetrics struct {
	mu sync.Mutex `json:"-"`

	WorkerBudget             int          `json:"worker_budget"`
	QueueTaskBudget          int          `json:"queue_task_budget"`
	PixelBufferPerThread     int          `json:"pixel_buffer_per_thread"`
	MaxActiveWorkersObserved int          `json:"max_active_workers_observed"`
	PeakQueuedTasksObserved  int          `json:"peak_queued_tasks_observed"`
	PeakThreadBufferPixels   int          `json:"peak_thread_buffer_pixels"`
	ProcessedTasks           int          `json:"processed_tasks"`
	QueueEvents              []QueueEvent `json:"queue_events"`
}

type BudgetMetricsSnapshot struct {
	WorkerBudget             int          `json:"worker_budget"`
	QueueTaskBudget          int          `json:"queue_task_budget"`
	PixelBufferPerThread     int          `json:"pixel_buffer_per_thread"`
	MaxActiveWorkersObserved int          `json:"max_active_workers_observed"`
	PeakQueuedTasksObserved  int          `json:"peak_queued_tasks_observed"`
	PeakThreadBufferPixels   int          `json:"peak_thread_buffer_pixels"`
	ProcessedTasks           int          `json:"processed_tasks"`
	QueueEvents              []QueueEvent `json:"queue_events"`
}

func NewBudgetMetrics(b model.Budget) *BudgetMetrics {
	return &BudgetMetrics{
		WorkerBudget:         maxInt(1, b.WorkerCount),
		QueueTaskBudget:      maxInt(1, b.PendingTaskQueueBudget),
		PixelBufferPerThread: b.PixelBufferPerThread,
	}
}

func (m *BudgetMetrics) observeStart(active, queued, bufferPixels int) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if active > m.MaxActiveWorkersObserved {
		m.MaxActiveWorkersObserved = active
	}
	if queued > m.PeakQueuedTasksObserved {
		m.PeakQueuedTasksObserved = queued
	}
	if bufferPixels > m.PeakThreadBufferPixels {
		m.PeakThreadBufferPixels = bufferPixels
	}
}

func (m *BudgetMetrics) taskFinished() {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.ProcessedTasks++
}

func (m *BudgetMetrics) recordQueue(event QueueEvent) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.QueueEvents = append(m.QueueEvents, event)
}

func (m *BudgetMetrics) Snapshot() BudgetMetricsSnapshot {
	m.mu.Lock()
	defer m.mu.Unlock()
	return BudgetMetricsSnapshot{
		WorkerBudget:             m.WorkerBudget,
		QueueTaskBudget:          m.QueueTaskBudget,
		PixelBufferPerThread:     m.PixelBufferPerThread,
		MaxActiveWorkersObserved: m.MaxActiveWorkersObserved,
		PeakQueuedTasksObserved:  m.PeakQueuedTasksObserved,
		PeakThreadBufferPixels:   m.PeakThreadBufferPixels,
		ProcessedTasks:           m.ProcessedTasks,
		QueueEvents:              append([]QueueEvent(nil), m.QueueEvents...),
	}
}

type Control struct {
	mu       sync.Mutex
	paused   bool
	canceled bool
}

func NewControl() *Control { return &Control{} }

func (c *Control) Pause() {
	c.mu.Lock()
	c.paused = true
	c.mu.Unlock()
}

func (c *Control) Resume() {
	c.mu.Lock()
	c.paused = false
	c.mu.Unlock()
}

func (c *Control) Cancel() {
	c.mu.Lock()
	c.canceled = true
	c.paused = false
	c.mu.Unlock()
}

func (c *Control) IsCanceled() bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.canceled
}

func (c *Control) Wait(ctx context.Context) error {
	for {
		c.mu.Lock()
		canceled := c.canceled
		paused := c.paused
		c.mu.Unlock()
		if canceled {
			return context.Canceled
		}
		if !paused {
			return nil
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(20 * time.Millisecond):
		}
	}
}

func maxInt(a, b int) int {
	if a > b {
		return a
	}
	return b
}
