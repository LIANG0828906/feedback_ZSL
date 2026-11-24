package pipeline

import (
	"fmt"
	"sync"
	"time"

	"cadpipe/internal/render"
)

type WaitRecord struct {
	TileID    int           `json:"tile_id"`
	Reason    string        `json:"reason"`
	WaitedFor time.Duration `json:"waited_for_ns"`
	At        time.Time     `json:"at"`
}

type BufferPool struct {
	buffers     chan *render.TileBuffer
	maxPixels   int
	waitMu      sync.Mutex
	WaitRecords []WaitRecord
}

func NewBufferPool(bufferCount, maxPixels int) *BufferPool {
	if bufferCount < 1 {
		bufferCount = 1
	}
	p := &BufferPool{
		buffers:   make(chan *render.TileBuffer, bufferCount),
		maxPixels: maxPixels,
	}
	for i := 0; i < bufferCount; i++ {
		p.buffers <- render.NewTileBuffer(maxPixels)
	}
	return p
}

func (p *BufferPool) MaxPixels() int { return p.maxPixels }

func (p *BufferPool) Acquire(tileID, requiredPixels int) (*BufferLease, error) {
	if requiredPixels > p.maxPixels {
		return nil, fmt.Errorf("tile %d requires %d pixels, per-thread buffer cap is %d",
			tileID, requiredPixels, p.maxPixels)
	}
	var waited time.Duration
	start := time.Now()
	select {
	case buf := <-p.buffers:
		waited = 0
		_ = start
		return &BufferLease{pool: p, buffer: resetBuffer(buf, requiredPixels)}, nil
	default:
	}

	reason := "pixel_buffer_exhausted"
	buf := <-p.buffers
	waited = time.Since(start)
	p.waitMu.Lock()
	p.WaitRecords = append(p.WaitRecords, WaitRecord{
		TileID: tileID, Reason: reason, WaitedFor: waited, At: time.Now(),
	})
	p.waitMu.Unlock()
	return &BufferLease{pool: p, buffer: resetBuffer(buf, requiredPixels)}, nil
}

func (p *BufferPool) SnapshotWaits() []WaitRecord {
	p.waitMu.Lock()
	defer p.waitMu.Unlock()
	return append([]WaitRecord(nil), p.WaitRecords...)
}

type BufferLease struct {
	pool   *BufferPool
	buffer *render.TileBuffer
}

func (l *BufferLease) Buffer() *render.TileBuffer { return l.buffer }

func (l *BufferLease) Release() {
	if l == nil || l.buffer == nil {
		return
	}
	l.pool.buffers <- l.buffer
	l.buffer = nil
}

func resetBuffer(buf *render.TileBuffer, pixels int) *render.TileBuffer {
	if cap(buf.Color) < pixels*4 || cap(buf.Depth) < pixels {
		return render.NewTileBuffer(pixels)
	}
	buf.Color = buf.Color[:pixels*4]
	buf.Depth = buf.Depth[:pixels]
	for i := 0; i < pixels; i++ {
		o := i * 4
		buf.Color[o+0] = 18
		buf.Color[o+1] = 22
		buf.Color[o+2] = 28
		buf.Color[o+3] = 255
		buf.Depth[i] = 1.7976931348623157e+308
	}
	return buf
}
