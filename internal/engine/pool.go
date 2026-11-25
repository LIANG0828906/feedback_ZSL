package engine

import (
	"sync"
	"sync/atomic"
)

// BlockRecord explains one blocking/backpressure episode. Nothing is ever
// silently dropped: when every per-worker buffer is full the submitter blocks
// after recording the reason.
type BlockRecord struct {
	SubmitSeq uint64 `json:"submit_seq"`
	TaskID    string `json:"task_id"`
	Worker    int    `json:"worker"`
	Reason    string `json:"reason"`
}

// Pool is a fixed-worker pool with a bounded per-worker channel buffer.
type Pool struct {
	workers   int
	buffer    int
	queues    []chan func()
	wg        sync.WaitGroup
	rr        uint64
	submitSeq uint64
	active    int32
	peak      int32
	completed int64
	blockMu   sync.Mutex
	blocks    []BlockRecord
	closeOnce sync.Once
}

// NewPool creates and starts a pool of workers goroutines each with buffer
// pending slots.
func NewPool(workers, buffer int) *Pool {
	if workers < 1 {
		workers = 1
	}
	if buffer < 1 {
		buffer = 1
	}
	p := &Pool{workers: workers, buffer: buffer}
	p.queues = make([]chan func(), workers)
	for i := 0; i < workers; i++ {
		p.queues[i] = make(chan func(), buffer)
		p.wg.Add(1)
		go p.loop(i)
	}
	return p
}

func (p *Pool) loop(id int) {
	defer p.wg.Done()
	for task := range p.queues[id] {
		cur := atomic.AddInt32(&p.active, 1)
		for {
			peak := atomic.LoadInt32(&p.peak)
			if cur <= peak || atomic.CompareAndSwapInt32(&p.peak, peak, cur) {
				break
			}
		}
		task()
		atomic.AddInt32(&p.active, -1)
		atomic.AddInt64(&p.completed, 1)
	}
}

// Submit enqueues a task, blocking (with a recorded reason) when all worker
// buffers are exhausted. Tasks are never dropped.
func (p *Pool) Submit(taskID string, task func()) {
	seq := atomic.AddUint64(&p.submitSeq, 1)
	start := int(atomic.AddUint64(&p.rr, 1)-1) % p.workers

	// Fast path: chosen worker has room.
	select {
	case p.queues[start] <- task:
		return
	default:
	}

	// Try any other worker without blocking.
	for off := 1; off < p.workers; off++ {
		w := (start + off) % p.workers
		select {
		case p.queues[w] <- task:
			return
		default:
		}
	}

	// All buffers full: record backpressure, then block on the RR worker.
	p.blockMu.Lock()
	p.blocks = append(p.blocks, BlockRecord{
		SubmitSeq: seq, TaskID: taskID, Worker: start, Reason: "worker_buffer_full",
	})
	p.blockMu.Unlock()
	p.queues[start] <- task
}

// Close closes queues and waits for all queued and running tasks to finish.
func (p *Pool) Close() {
	p.closeOnce.Do(func() {
		for _, q := range p.queues {
			close(q)
		}
	})
	p.wg.Wait()
}

// Blocks returns a copy of recorded backpressure episodes.
func (p *Pool) Blocks() []BlockRecord {
	p.blockMu.Lock()
	defer p.blockMu.Unlock()
	return append([]BlockRecord(nil), p.blocks...)
}

// PeakParallel reports the maximum number of concurrently running tasks.
func (p *Pool) PeakParallel() int { return int(atomic.LoadInt32(&p.peak)) }

// Completed reports the number of fully executed tasks.
func (p *Pool) Completed() int { return int(atomic.LoadInt64(&p.completed)) }

// MaxParallel returns the configured parallelism bound.
func (p *Pool) MaxParallel() int { return p.workers }

// BufferPerWorker returns the configured per-thread buffer cap.
func (p *Pool) BufferPerWorker() int { return p.buffer }
