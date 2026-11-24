package core

import (
	"context"
	"fmt"
	"sync"
	"sync/atomic"
	"time"
)

type WaitEvent struct {
	Label    string `json:"label"`
	Reason   string `json:"reason"`
	Pixels   int    `json:"pixels"`
	AtUnixMS int64  `json:"atUnixMs"`
}

type PoolStats struct {
	MaxParallel     int            `json:"maxParallel"`
	WorkerCount     int            `json:"workerCount"`
	PerWorkerPixels int            `json:"perWorkerPixels"`
	BufferBytes     int            `json:"bufferBytes"`
	WaitEvents      []WaitEvent    `json:"waitEvents"`
	WaitReasons     map[string]int `json:"waitReasons"`
}

type poolJob struct {
	label  string
	pixels int
	fn     func(buffer []byte) error
}

type WorkerPool struct {
	maxWorkers      int
	perWorkerPixels int
	jobs            chan poolJob
	done            chan struct{}
	wg              sync.WaitGroup

	mu           sync.Mutex
	waitEvents   []WaitEvent
	waitReasons  map[string]int
	active       int64
	maxObserved  int64
	bufferTokens chan struct{}
}

func NewWorkerPool(maxWorkers, perWorkerPixels int) (*WorkerPool, error) {
	if maxWorkers < 1 || maxWorkers > 64 {
		return nil, NewError(CodeParam, "workers", "worker count must be in [1,64]")
	}
	if perWorkerPixels < 4096 || perWorkerPixels > 64*1024*1024 {
		return nil, NewError(CodeParam, "perWorkerPixels", "per-worker pixel buffer must be in [4096,67108864]")
	}
	total := maxWorkers * perWorkerPixels
	p := &WorkerPool{
		maxWorkers:      maxWorkers,
		perWorkerPixels: perWorkerPixels,
		jobs:            make(chan poolJob),
		done:            make(chan struct{}),
		waitReasons:     map[string]int{},
		bufferTokens:    make(chan struct{}, total),
	}
	for i := 0; i < total; i++ {
		p.bufferTokens <- struct{}{}
	}
	for i := 0; i < maxWorkers; i++ {
		p.wg.Add(1)
		go p.worker()
	}
	return p, nil
}

func (p *WorkerPool) worker() {
	defer p.wg.Done()
	buffer := make([]byte, p.perWorkerPixels)
	for job := range p.jobs {
		current := atomic.AddInt64(&p.active, 1)
		for {
			observed := atomic.LoadInt64(&p.maxObserved)
			if current <= observed || atomic.CompareAndSwapInt64(&p.maxObserved, observed, current) {
				break
			}
		}
		func() {
			defer func() {
				_ = recover()
				atomic.AddInt64(&p.active, -1)
			}()
			_ = job.fn(buffer)
		}()
	}
}

func (p *WorkerPool) Submit(ctx context.Context, label string, pixels int, fn func([]byte) error) error {
	if pixels <= 0 {
		pixels = 1
	}
	need := min(pixels, p.perWorkerPixels)
	acquired := 0
	for acquired < need {
		select {
		case <-p.bufferTokens:
			acquired++
		default:
			p.recordWait(label, "pixel-buffer-exhausted", pixels)
			select {
			case <-p.bufferTokens:
				acquired++
			case <-ctx.Done():
				return WrapError(CodeCanceled, label, "canceled while waiting for pixel buffer", ctx.Err())
			case <-p.done:
				return NewError(CodeCanceled, label, "worker pool closed")
			}
		}
	}
	defer func() {
		for i := 0; i < acquired; i++ {
			p.bufferTokens <- struct{}{}
		}
	}()
	select {
	case p.jobs <- poolJob{label: label, pixels: pixels, fn: fn}:
		return nil
	default:
		p.recordWait(label, "worker-slot-exhausted", pixels)
	}
	select {
	case p.jobs <- poolJob{label: label, pixels: pixels, fn: fn}:
		return nil
	case <-ctx.Done():
		return WrapError(CodeCanceled, label, "canceled while queued for a worker", ctx.Err())
	case <-p.done:
		return NewError(CodeCanceled, label, "worker pool closed")
	}
}

func (p *WorkerPool) recordWait(label, reason string, pixels int) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if len(p.waitEvents) < 512 {
		p.waitEvents = append(p.waitEvents, WaitEvent{
			Label:    label,
			Reason:   reason,
			Pixels:   pixels,
			AtUnixMS: time.Now().UnixMilli(),
		})
	}
	p.waitReasons[reason]++
}

func (p *WorkerPool) Stats() PoolStats {
	p.mu.Lock()
	defer p.mu.Unlock()
	events := append([]WaitEvent(nil), p.waitEvents...)
	reasons := make(map[string]int, len(p.waitReasons))
	for k, v := range p.waitReasons {
		reasons[k] = v
	}
	return PoolStats{
		MaxParallel:     int(atomic.LoadInt64(&p.maxObserved)),
		WorkerCount:     p.maxWorkers,
		PerWorkerPixels: p.perWorkerPixels,
		BufferBytes:     p.maxWorkers * p.perWorkerPixels,
		WaitEvents:      events,
		WaitReasons:     reasons,
	}
}

func (p *WorkerPool) Close() {
	select {
	case <-p.done:
		return
	default:
		close(p.done)
	}
	close(p.jobs)
	p.wg.Wait()
}

func (p *WorkerPool) String() string {
	return fmt.Sprintf("WorkerPool{max=%d, buffer=%d}", p.maxWorkers, p.perWorkerPixels)
}
