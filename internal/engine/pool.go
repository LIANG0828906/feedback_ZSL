package engine

import (
	"context"
	"fmt"
	"runtime/debug"
	"sort"
	"sync"
	"sync/atomic"

	"retargetlab/internal/solver"
)

type poolConfig struct {
	workers         int
	queuePerWorker  int
	bufferPerWorker int
}

type poolStats struct {
	Workers           int   `json:"workers"`
	QueueCapacity     int   `json:"queueCapacity"`
	BufferLimit       int   `json:"bufferLimit"`
	PeakQueue         int   `json:"peakQueue"`
	PeakInFlight      int   `json:"peakInFlight"`
	BlockedDispatches int64 `json:"blockedDispatches"`
}

type frameOutcome struct {
	frame  int
	result solver.FrameResult
	err    error
}

func (e *Engine) solveWithPool(ctx context.Context, solve func(context.Context, int) (solver.FrameResult, error), frames []int) ([]frameOutcome, poolStats, error) {
	cfg := poolConfig{
		workers:         e.maxWorkers,
		queuePerWorker:  e.queuePerWorker,
		bufferPerWorker: e.bufferPerWorker,
	}
	if cfg.workers < 1 {
		cfg.workers = 1
	}
	if cfg.workers > len(frames) {
		cfg.workers = len(frames)
	}
	if cfg.workers < 1 {
		return nil, poolStats{}, nil
	}
	if cfg.queuePerWorker < 1 {
		cfg.queuePerWorker = 1
	}
	if cfg.bufferPerWorker < 1 {
		cfg.bufferPerWorker = 1
	}
	queueCap := cfg.workers * cfg.queuePerWorker
	bufferCap := cfg.workers * cfg.bufferPerWorker
	jobs := make(chan int, queueCap)
	results := make(chan frameOutcome, len(frames))
	buffers := make(chan struct{}, bufferCap)
	stats := poolStats{Workers: cfg.workers, QueueCapacity: queueCap, BufferLimit: bufferCap}

	var peakQueue int64
	var peakInFlight int64
	var inFlight int64
	var blocked int64
	producerDone := make(chan struct{})
	go func() {
		defer close(producerDone)
		defer close(jobs)
		for _, frame := range frames {
			select {
			case jobs <- frame:
			case <-ctx.Done():
				return
			default:
				select {
				case jobs <- frame:
				case <-ctx.Done():
					return
				}
				atomic.AddInt64(&blocked, 1)
			}
			for {
				n := int64(len(jobs))
				old := atomic.LoadInt64(&peakQueue)
				if n <= old || atomic.CompareAndSwapInt64(&peakQueue, old, n) {
					break
				}
			}
		}
	}()

	var wg sync.WaitGroup
	for worker := 0; worker < cfg.workers; worker++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for frame := range jobs {
				select {
				case buffers <- struct{}{}:
				case <-ctx.Done():
					results <- frameOutcome{frame: frame, err: ctx.Err()}
					continue
				}
				cur := atomic.AddInt64(&inFlight, 1)
				for {
					old := atomic.LoadInt64(&peakInFlight)
					if cur <= old || atomic.CompareAndSwapInt64(&peakInFlight, old, cur) {
						break
					}
				}
				out := safeSolve(ctx, solve, frame)
				atomic.AddInt64(&inFlight, -1)
				<-buffers
				results <- out
			}
		}()
	}
	go func() {
		<-producerDone
		wg.Wait()
		close(results)
	}()

	var out []frameOutcome
	for result := range results {
		out = append(out, result)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].frame < out[j].frame })
	stats.PeakQueue = int(atomic.LoadInt64(&peakQueue))
	stats.PeakInFlight = int(atomic.LoadInt64(&peakInFlight))
	stats.BlockedDispatches = atomic.LoadInt64(&blocked)
	if err := ctx.Err(); err != nil {
		return out, stats, err
	}
	return out, stats, nil
}

func safeSolve(ctx context.Context, solve func(context.Context, int) (solver.FrameResult, error), frame int) (out frameOutcome) {
	out.frame = frame
	defer func() {
		if r := recover(); r != nil {
			out.err = fmt.Errorf("frame solver panic: %v\n%s", r, debug.Stack())
		}
	}()
	out.result, out.err = solve(ctx, frame)
	return out
}
