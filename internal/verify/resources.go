package verify

import (
	"context"
	"errors"
	"sync"
	"time"
)

type BufferLimiter struct {
	mu          sync.Mutex
	cond        *sync.Cond
	capacity    int64
	outstanding int64
	waits       []QueueWait
}

func NewBufferLimiter(parallelism int, perThreadPixels int64) *BufferLimiter {
	l := &BufferLimiter{capacity: int64(parallelism) * perThreadPixels}
	l.cond = sync.NewCond(&l.mu)
	return l
}

type Lease struct {
	l      *BufferLimiter
	pixels int64
}

func (l *BufferLimiter) Acquire(ctx context.Context, pixels int64, frame int, typ EvidenceType, reason string) (*Lease, error) {
	if pixels > l.capacity {
		return nil, errors.New("pixel buffer request exceeds configured buffer capacity")
	}
	start := time.Now()
	waited := false
	l.mu.Lock()
	for l.outstanding+pixels > l.capacity {
		waited = true
		done := make(chan struct{})
		go func() {
			select {
			case <-ctx.Done():
				l.cond.Broadcast()
			case <-done:
			}
		}()
		l.cond.Wait()
		close(done)
		if err := ctx.Err(); err != nil {
			l.mu.Unlock()
			return nil, err
		}
	}
	l.outstanding += pixels
	waitMS := time.Since(start).Milliseconds()
	if waited || waitMS > 0 {
		l.waits = append(l.waits, QueueWait{
			Frame: frame, EvidenceType: typ, Pixels: pixels,
			Reason: reason, WaitedMS: waitMS,
		})
	}
	l.mu.Unlock()
	return &Lease{l: l, pixels: pixels}, nil
}

func (l *BufferLimiter) release(pixels int64) {
	l.mu.Lock()
	l.outstanding -= pixels
	if l.outstanding < 0 {
		l.outstanding = 0
	}
	l.cond.Broadcast()
	l.mu.Unlock()
}

func (x *Lease) Release() {
	if x != nil && x.l != nil {
		x.l.release(x.pixels)
	}
}

func (l *BufferLimiter) Waits() []QueueWait {
	l.mu.Lock()
	defer l.mu.Unlock()
	return append([]QueueWait(nil), l.waits...)
}

type workerPool struct {
	sem chan struct{}
	wg  sync.WaitGroup
}

func newWorkerPool(n int) *workerPool {
	return &workerPool{sem: make(chan struct{}, n)}
}

func (p *workerPool) Go(fn func()) {
	p.wg.Add(1)
	p.sem <- struct{}{}
	go func() {
		defer p.wg.Done()
		defer func() { <-p.sem }()
		fn()
	}()
}

func (p *workerPool) Wait() { p.wg.Wait() }
