package retarget

import (
	"context"
	"sync"
	"sync/atomic"
	"time"

	"skeletonretarget/internal/model"
)

type QueueStats struct {
	Submitted     int64
	FullWaits     int64
	WaitNanos     int64
	MaxQueueDepth int
	WorkerCount   int
	MaxJointState int
	Waits         []QueueWait `json:"waits,omitempty"`
}

type QueueWait struct {
	Label     string `json:"label"`
	Reason    string `json:"reason"`
	WaitNanos int64  `json:"waitNanos"`
}

type job struct {
	bufferSize int
	label      string
	run        func() error
}

type workerPool struct {
	jobs        chan job
	wg          sync.WaitGroup
	closeOnce   sync.Once
	jointLimit  int
	submitted   atomic.Int64
	fullWaits   atomic.Int64
	waitNanos   atomic.Int64
	maxQueue    atomic.Int64
	maxJointBuf atomic.Int64
	waitMu      sync.Mutex
	waitEvents  []QueueWait
}

func newWorkerPool(workers, queueBudget, jointLimit int) *workerPool {
	if workers < 1 {
		workers = 1
	}
	if queueBudget < 1 {
		queueBudget = 1
	}
	p := &workerPool{jobs: make(chan job, queueBudget), jointLimit: jointLimit}
	for i := 0; i < workers; i++ {
		p.wg.Add(1)
		go func() {
			defer p.wg.Done()
			for j := range p.jobs {
				if j.bufferSize > 0 {
					for {
						current := p.maxJointBuf.Load()
						if int64(j.bufferSize) <= current || p.maxJointBuf.CompareAndSwap(current, int64(j.bufferSize)) {
							break
						}
					}
				}
				_ = j.run()
			}
		}()
	}
	return p
}

func (p *workerPool) submit(ctx context.Context, j job) error {
	if p.jointLimit > 0 && j.bufferSize > p.jointLimit {
		return &model.Error{Code: model.CodeFrame, Frame: -1, Message: "joint state buffer exceeds worker budget"}
	}
	depth := int64(len(p.jobs))
	for {
		current := p.maxQueue.Load()
		if depth <= current || p.maxQueue.CompareAndSwap(current, depth) {
			break
		}
	}
	select {
	case p.jobs <- j:
		p.submitted.Add(1)
		return nil
	default:
	}
	start := time.Now()
	p.fullWaits.Add(1)
	select {
	case p.jobs <- j:
		waited := time.Since(start).Nanoseconds()
		p.waitNanos.Add(waited)
		p.waitMu.Lock()
		p.waitEvents = append(p.waitEvents, QueueWait{Label: j.label, Reason: "frame_queue_budget_exhausted", WaitNanos: waited})
		p.waitMu.Unlock()
		p.submitted.Add(1)
		return nil
	case <-ctx.Done():
		waited := time.Since(start).Nanoseconds()
		p.waitNanos.Add(waited)
		p.waitMu.Lock()
		p.waitEvents = append(p.waitEvents, QueueWait{Label: j.label, Reason: "context_cancelled_while_queued", WaitNanos: waited})
		p.waitMu.Unlock()
		return ctx.Err()
	}
}

func (p *workerPool) close() {
	p.closeOnce.Do(func() {
		close(p.jobs)
		p.wg.Wait()
	})
}

func (p *workerPool) stats() QueueStats {
	p.waitMu.Lock()
	waits := append([]QueueWait(nil), p.waitEvents...)
	p.waitMu.Unlock()
	return QueueStats{
		Submitted: p.submitted.Load(), FullWaits: p.fullWaits.Load(), WaitNanos: p.waitNanos.Load(),
		MaxQueueDepth: int(p.maxQueue.Load()), MaxJointState: int(p.maxJointBuf.Load()), Waits: waits,
	}
}
