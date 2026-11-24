package resource

import (
	"context"
	"sync"
)

type Gate struct {
	mu         sync.Mutex
	cond       *sync.Cond
	pixelFree  int64
	sampleFree int64
	pixelCap   int64
	sampleCap  int64
}

type Lease struct {
	pixels  int64
	samples int64
	gate    *Gate
}

func New(maxPixelsPerWorker, maxSamplesPerWorker int64, workers int) *Gate {
	if workers < 1 {
		workers = 1
	}
	g := &Gate{
		pixelCap:  maxPixelsPerWorker * int64(workers),
		sampleCap: maxSamplesPerWorker * int64(workers),
	}
	g.pixelFree = g.pixelCap
	g.sampleFree = g.sampleCap
	g.cond = sync.NewCond(&g.mu)
	return g
}

func (g *Gate) Acquire(ctx context.Context, pixels, samples int64) (*Lease, bool, error) {
	if pixels < 0 {
		pixels = 0
	}
	if samples < 0 {
		samples = 0
	}
	if pixels > g.pixelCap {
		pixels = g.pixelCap
	}
	if samples > g.sampleCap {
		samples = g.sampleCap
	}
	done := make(chan struct{})
	go func() {
		select {
		case <-ctx.Done():
			g.mu.Lock()
			g.cond.Broadcast()
			g.mu.Unlock()
		case <-done:
		}
	}()
	defer close(done)
	g.mu.Lock()
	defer g.mu.Unlock()
	waited := false
	notified := false
	for {
		if ctx.Err() != nil {
			return nil, waited, ctx.Err()
		}
		if pixels <= g.pixelFree && samples <= g.sampleFree {
			g.pixelFree -= pixels
			g.sampleFree -= samples
			return &Lease{pixels: pixels, samples: samples, gate: g}, waited, nil
		}
		if !notified {
			waited = true
			notified = true
		}
		g.cond.Wait()
	}
}

func (l *Lease) Release() {
	if l == nil || l.gate == nil {
		return
	}
	g := l.gate
	g.mu.Lock()
	g.pixelFree += l.pixels
	g.sampleFree += l.samples
	if g.pixelFree > g.pixelCap {
		g.pixelFree = g.pixelCap
	}
	if g.sampleFree > g.sampleCap {
		g.sampleFree = g.sampleCap
	}
	g.cond.Broadcast()
	g.mu.Unlock()
}

func (g *Gate) Capacity() (int64, int64) {
	return g.pixelCap, g.sampleCap
}
