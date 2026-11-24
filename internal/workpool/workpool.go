package workpool

import (
	"context"
	"sync"
	"sync/atomic"

	"gameintegrity/internal/apperr"
)

// BufferPool is a blocking semaphore for pixel buffers. Acquire waits rather
// than dropping work when the pool is exhausted.
type BufferPool struct {
	slots     int
	frameSize int64
	sem       chan struct{}
	waits     atomic.Int64
}

func NewBufferPool(maxBytes, frameBytes int64) *BufferPool {
	if frameBytes <= 0 {
		frameBytes = 1
	}
	slots := int(maxBytes / frameBytes)
	if slots < 1 {
		slots = 1
	}
	return &BufferPool{
		slots: slots, frameSize: frameBytes,
		sem: make(chan struct{}, slots),
	}
}

func (p *BufferPool) Slots() int        { return p.slots }
func (p *BufferPool) FrameBytes() int64 { return p.frameSize }
func (p *BufferPool) Waits() int64      { return p.waits.Load() }

func (p *BufferPool) Acquire(ctx context.Context, n int) error {
	if n < 1 {
		return nil
	}
	if n > p.slots {
		e := apperr.New(apperr.Para, "pixel buffer pool is smaller than one analysis working set")
		e.Field = "max_pixel_buffer_bytes"
		e.Expected = int64(n) * p.frameSize
		e.Actual = int64(p.slots) * p.frameSize
		e.AvailableRange = "at least one frame plus history neighbors"
		return e
	}
	acquired := 0
	waited := false
	for acquired < n {
		select {
		case p.sem <- struct{}{}:
			acquired++
		case <-ctx.Done():
			for i := 0; i < acquired; i++ {
				<-p.sem
			}
			return ctx.Err()
		default:
			if !waited {
				p.waits.Add(1)
				waited = true
			}
			select {
			case p.sem <- struct{}{}:
				acquired++
			case <-ctx.Done():
				for i := 0; i < acquired; i++ {
					<-p.sem
				}
				return ctx.Err()
			}
		}
	}
	return nil
}

func (p *BufferPool) Release(n int) {
	for i := 0; i < n; i++ {
		<-p.sem
	}
}

func Run(ctx context.Context, workers, tasks int, fn func(context.Context, int) error) error {
	if tasks <= 0 {
		return nil
	}
	if workers < 1 {
		workers = 1
	}
	if workers > tasks {
		workers = tasks
	}
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	jobs := make(chan int)
	var firstErr error
	var errMu sync.Mutex
	var wg sync.WaitGroup
	for w := 0; w < workers; w++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for {
				select {
				case <-ctx.Done():
					return
				case task, ok := <-jobs:
					if !ok {
						return
					}
					if err := fn(ctx, task); err != nil {
						errMu.Lock()
						if firstErr == nil {
							firstErr = err
						}
						errMu.Unlock()
						cancel()
						return
					}
				}
			}
		}()
	}
	go func() {
		defer close(jobs)
		for i := 0; i < tasks; i++ {
			select {
			case jobs <- i:
			case <-ctx.Done():
				return
			}
		}
	}()
	wg.Wait()
	errMu.Lock()
	defer errMu.Unlock()
	if firstErr != nil {
		return firstErr
	}
	return ctx.Err()
}
