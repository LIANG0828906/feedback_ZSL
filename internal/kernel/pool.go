package kernel

import (
	"context"
	"sync"
	"sync/atomic"
)

type PoolAuditEvent struct {
	Sequence      int64  `json:"sequence"`
	TileID        string `json:"tile_id"`
	Event         string `json:"event"`
	Reason        string `json:"reason,omitempty"`
	ActiveWorkers int32  `json:"active_workers"`
	MaxWorkers    int    `json:"max_workers"`
	BufferBytes   int    `json:"buffer_bytes,omitempty"`
	BufferLimit   int    `json:"buffer_limit_bytes"`
}

type TileJob struct {
	TileID        string
	EstimateBytes int
	Run           func(buffer []byte) (*TileResult, error)
}

type TileJobResult struct {
	TileID string
	Result *TileResult
	Err    error
}

type BoundedPool struct {
	MaxWorkers          int
	GeometryBufferLimit int

	mu        sync.Mutex
	acquireMu sync.Mutex
	seq       int64
	active    int32
	Audit     []PoolAuditEvent
}

func NewBoundedPool(maxWorkers, bufferLimit int) *BoundedPool {
	if maxWorkers <= 0 {
		maxWorkers = 1
	}
	if bufferLimit <= 0 {
		bufferLimit = 64 * 1024
	}
	return &BoundedPool{MaxWorkers: maxWorkers, GeometryBufferLimit: bufferLimit}
}

func (p *BoundedPool) record(tileID, event, reason string, bufferBytes int) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.seq++
	p.Audit = append(p.Audit, PoolAuditEvent{
		Sequence:      p.seq,
		TileID:        tileID,
		Event:         event,
		Reason:        reason,
		ActiveWorkers: atomic.LoadInt32(&p.active),
		MaxWorkers:    p.MaxWorkers,
		BufferBytes:   bufferBytes,
		BufferLimit:   p.GeometryBufferLimit,
	})
}

func (p *BoundedPool) AuditEvents() []PoolAuditEvent {
	p.mu.Lock()
	defer p.mu.Unlock()
	return append([]PoolAuditEvent{}, p.Audit...)
}

func (p *BoundedPool) Run(ctx context.Context, jobs []TileJob) []TileJobResult {
	results := make([]TileJobResult, len(jobs))
	semaphore := make(chan struct{}, p.MaxWorkers)
	var wg sync.WaitGroup

	for i, job := range jobs {
		i := i
		job := job
		wg.Add(1)
		go func() {
			defer wg.Done()
			p.acquireMu.Lock()
			if atomic.LoadInt32(&p.active) >= int32(p.MaxWorkers) {
				p.record(job.TileID, "queued", "worker_pool_exhausted", job.EstimateBytes)
			}
			select {
			case semaphore <- struct{}{}:
			case <-ctx.Done():
				p.acquireMu.Unlock()
				results[i] = TileJobResult{TileID: job.TileID, Err: ctx.Err()}
				return
			}
			atomic.AddInt32(&p.active, 1)
			p.record(job.TileID, "started", "acquired_worker", job.EstimateBytes)
			p.acquireMu.Unlock()
			defer func() {
				atomic.AddInt32(&p.active, -1)
				<-semaphore
				p.record(job.TileID, "finished", "released_worker", job.EstimateBytes)
			}()

			if job.EstimateBytes > p.GeometryBufferLimit {
				err := blockError(job.TileID, "geometry buffer estimate %d exceeds per-thread limit %d",
					job.EstimateBytes, p.GeometryBufferLimit)
				p.record(job.TileID, "rejected", "geometry_buffer_limit", job.EstimateBytes)
				results[i] = TileJobResult{TileID: job.TileID, Err: err}
				return
			}
			bufferSize := job.EstimateBytes
			if bufferSize <= 0 {
				bufferSize = 4096
			}
			if bufferSize > p.GeometryBufferLimit {
				bufferSize = p.GeometryBufferLimit
			}
			buffer := make([]byte, bufferSize)
			result, err := job.Run(buffer)
			results[i] = TileJobResult{TileID: job.TileID, Result: result, Err: err}
		}()
	}
	wg.Wait()
	return results
}
