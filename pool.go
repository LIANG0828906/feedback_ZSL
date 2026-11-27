package grid

import (
	"context"
	"sync"
	"time"
)

type Ray struct {
	Tile  TileID
	Index int
	Name  string
}

type RayProcessor func(context.Context, Ray) error

type PoolConfig struct {
	MaxParallelism int           `json:"maxParallelism"`
	RayBufferLimit int           `json:"rayBufferLimit"`
	RayDelay       time.Duration `json:"-"`
}

type WaitRecord struct {
	Reason   string  `json:"reason"`
	WorkerID int     `json:"workerId"`
	WaitedNS int64   `json:"waitedNs"`
	WaitedMS float64 `json:"waited"`
}

type PoolStats struct {
	MaxParallelism int          `json:"maxParallelism"`
	RayBufferLimit int          `json:"rayBufferLimit"`
	PeakParallel   int          `json:"peakParallel"`
	Waits          []WaitRecord `json:"waits"`
	Completed      int64        `json:"completed"`
}

type rayJob struct {
	ray  Ray
	done func()
}

type RayPool struct {
	cfg       PoolConfig
	processor RayProcessor
	idle      chan int
	buffers   []chan rayJob
	workerWG  []sync.WaitGroup
	drainWG   sync.WaitGroup

	mu        sync.Mutex
	peak      int
	active    int
	waits     []WaitRecord
	completed int64
}

func NewRayPool(cfg PoolConfig, processor RayProcessor) *RayPool {
	if cfg.MaxParallelism <= 0 {
		cfg.MaxParallelism = 4
	}
	if cfg.RayBufferLimit <= 0 {
		cfg.RayBufferLimit = 4
	}
	p := &RayPool{
		cfg:       cfg,
		processor: processor,
		idle:      make(chan int, cfg.MaxParallelism),
		buffers:   make([]chan rayJob, cfg.MaxParallelism),
		workerWG:  make([]sync.WaitGroup, cfg.MaxParallelism),
	}
	for i := 0; i < cfg.MaxParallelism; i++ {
		p.idle <- i
		p.buffers[i] = make(chan rayJob, cfg.RayBufferLimit)
		p.drainWG.Add(1)
		go p.drain(i, p.buffers[i])
	}
	return p
}

func (p *RayPool) drain(workerID int, ch <-chan rayJob) {
	defer p.drainWG.Done()
	for job := range ch {
		if p.processor != nil {
			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			_ = p.processor(ctx, job.ray)
			cancel()
		}
		if p.cfg.RayDelay > 0 {
			time.Sleep(p.cfg.RayDelay)
		}
		p.mu.Lock()
		p.completed++
		p.mu.Unlock()
		job.done()
	}
}

// Shutdown closes ray buffers and waits for all drain goroutines.
func (p *RayPool) Shutdown() {
	for _, ch := range p.buffers {
		close(ch)
	}
	p.drainWG.Wait()
}

func (p *RayPool) acquire(ctx context.Context, requestedID int) (int, error) {
	select {
	case id := <-p.idle:
		p.setActive(1)
		return id, nil
	default:
	}
	start := time.Now()
	var id int
	select {
	case id = <-p.idle:
	case <-ctx.Done():
		return -1, ctx.Err()
	}
	waited := time.Since(start)
	p.recordWait(WaitRecord{Reason: "parallel-slot-full", WorkerID: requestedID, WaitedNS: waited.Nanoseconds(), WaitedMS: float64(waited.Microseconds())})
	p.setActive(1)
	return id, nil
}

func (p *RayPool) release(id int) {
	p.setActive(-1)
	p.idle <- id
}

func (p *RayPool) setActive(delta int) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.active += delta
	if p.active > p.peak {
		p.peak = p.active
	}
}

func (p *RayPool) recordWait(w WaitRecord) {
	p.mu.Lock()
	p.waits = append(p.waits, w)
	p.mu.Unlock()
}

// SubmitRay enqueues one ray into the calling worker's bounded buffer. If the
// buffer is full, the caller blocks and the wait is recorded; rays are never
// silently dropped.
func (p *RayPool) SubmitRay(ctx context.Context, workerID int, ray Ray) error {
	if workerID < 0 || workerID >= len(p.buffers) {
		workerID = 0
	}
	p.workerWG[workerID].Add(1)
	job := rayJob{ray: ray, done: func() { p.workerWG[workerID].Done() }}
	ch := p.buffers[workerID]

	select {
	case ch <- job:
		return nil
	default:
	}
	start := time.Now()
	select {
	case ch <- job:
	case <-ctx.Done():
		p.workerWG[workerID].Done()
		return ctx.Err()
	}
	waited := time.Since(start)
	p.recordWait(WaitRecord{
		Reason: "ray-buffer-exhausted", WorkerID: workerID,
		WaitedNS: waited.Nanoseconds(), WaitedMS: float64(waited.Microseconds()),
	})
	return nil
}

func (p *RayPool) Flush(workerID int) {
	if workerID >= 0 && workerID < len(p.workerWG) {
		p.workerWG[workerID].Wait()
	}
}

func (p *RayPool) Stats() PoolStats {
	p.mu.Lock()
	defer p.mu.Unlock()
	waits := append([]WaitRecord(nil), p.waits...)
	return PoolStats{
		MaxParallelism: p.cfg.MaxParallelism, RayBufferLimit: p.cfg.RayBufferLimit,
		PeakParallel: p.peak, Waits: waits, Completed: p.completed,
	}
}

type TileJob func(ctx context.Context, workerID int) error

// RunJobs runs at most MaxParallelism jobs and returns the first structured
// tile error while allowing all queued jobs to finish their resource cleanup.
func (p *RayPool) RunJobs(ctx context.Context, jobs []TileJob) error {
	if ctx == nil {
		ctx = context.Background()
	}
	runCtx, cancel := context.WithCancel(ctx)
	defer cancel()
	var wg sync.WaitGroup
	var errMu sync.Mutex
	var firstErr error

	for i, job := range jobs {
		if job == nil {
			continue
		}
		wg.Add(1)
		go func(requestedID int, job TileJob) {
			defer wg.Done()
			workerID, err := p.acquire(runCtx, requestedID)
			if err != nil {
				return
			}
			defer p.release(workerID)
			if err := job(runCtx, workerID); err != nil {
				errMu.Lock()
				if firstErr == nil {
					firstErr = err
					cancel()
				}
				errMu.Unlock()
			}
		}(i, job)
	}
	wg.Wait()
	return firstErr
}

// BlockError builds the stable E_BLOCK injection/diagnostic error.
func BlockError(id TileID, obstacleID, field, message string) *Error {
	return &Error{Code: CodeBlock, Tile: &id, ObjectID: obstacleID, Field: field, Message: message}
}
