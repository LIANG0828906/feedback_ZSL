package silhouette

import (
	"context"
	"strconv"
	"sync"
	"time"
)

const (
	DefaultWorkers           = 3
	DefaultVerticesPerWorker = 8192
)

type WorkerPoolConfig struct {
	Workers           int
	VerticesPerWorker int
}

type WorkerEvent struct {
	TaskID         string `json:"task_id"`
	WorkerID       int    `json:"worker_id"`
	WaitNanos      int64  `json:"wait_nanos"`
	ElapsedNanos   int64  `json:"elapsed_nanos"`
	Reason         string `json:"reason,omitempty"`
	BufferCapacity int    `json:"buffer_capacity"`
	Vertices       int    `json:"vertices"`
}

type workerSlot struct {
	id            int
	verticesLimit int
}

type WorkerPool struct {
	slots  chan *workerSlot
	closed chan struct{}
	mu     sync.Mutex
	events []WorkerEvent
}

func NewWorkerPool(cfg WorkerPoolConfig) (*WorkerPool, error) {
	if cfg.Workers <= 0 {
		cfg.Workers = DefaultWorkers
	}
	if cfg.VerticesPerWorker <= 0 {
		cfg.VerticesPerWorker = DefaultVerticesPerWorker
	}
	p := &WorkerPool{slots: make(chan *workerSlot, cfg.Workers), closed: make(chan struct{})}
	for i := 0; i < cfg.Workers; i++ {
		p.slots <- &workerSlot{id: i, verticesLimit: cfg.VerticesPerWorker}
	}
	return p, nil
}

// Do blocks when every worker is busy. The wait is recorded with a reason and
// duration, satisfying bounded parallelism and buffer-exhaustion diagnostics.
func (p *WorkerPool) Do(ctx context.Context, taskID string, estimatedVertices int, fn func(workerID, bufferLimit int) (int, error)) error {
	start := time.Now()
	waitEnd := start
	var slot *workerSlot
	select {
	case slot = <-p.slots:
		waitEnd = time.Now()
	default:
		select {
		case slot = <-p.slots:
			waitEnd = time.Now()
		case <-ctx.Done():
			p.record(WorkerEvent{TaskID: taskID, WaitNanos: int64(time.Since(start)), Reason: "context_cancelled_while_queued"})
			return ctx.Err()
		}
	}
	defer func() { p.slots <- slot }()
	if estimatedVertices > slot.verticesLimit {
		p.record(WorkerEvent{TaskID: taskID, WorkerID: slot.id, WaitNanos: int64(waitEnd.Sub(start)), ElapsedNanos: int64(time.Since(start)), Reason: "estimated vertices exceed per-worker buffer", BufferCapacity: slot.verticesLimit, Vertices: estimatedVertices})
		return (&DomainError{Code: CodeBuffer, Path: "$.worker.buffer", Message: "per-worker vertex buffer exhausted before scheduling"}).
			WithDetail("worker_id", strconv.Itoa(slot.id)).
			WithDetail("task", taskID).
			WithDetail("vertices", strconv.Itoa(estimatedVertices)).
			WithDetail("capacity", strconv.Itoa(slot.verticesLimit))
	}
	vertices, err := fn(slot.id, slot.verticesLimit)
	wait := int64(waitEnd.Sub(start))
	event := WorkerEvent{
		TaskID: taskID, WorkerID: slot.id, WaitNanos: wait, ElapsedNanos: int64(time.Since(start)),
		BufferCapacity: slot.verticesLimit, Vertices: vertices,
	}
	if wait > 0 {
		event.Reason = "blocked in queue while worker parallelism and per-worker vertex buffers were exhausted"
	}
	p.record(event)
	return err
}

func (p *WorkerPool) record(event WorkerEvent) {
	p.mu.Lock()
	p.events = append(p.events, event)
	p.mu.Unlock()
}

func (p *WorkerPool) Events() []WorkerEvent {
	p.mu.Lock()
	defer p.mu.Unlock()
	return append([]WorkerEvent(nil), p.events...)
}

func (p *WorkerPool) Close() {
	select {
	case <-p.closed:
	default:
		close(p.closed)
	}
}
