package maskqa

import (
	"context"
	"fmt"
	"runtime"
	"sync"
	"time"
)

type SchedulerConfig struct {
	MaxParallelism     int `json:"max_parallelism"`
	MaxPixelsPerWorker int `json:"max_pixels_per_worker"`
	QueueBudgetTiles   int `json:"queue_budget_tiles"`
}

func DefaultSchedulerConfig() SchedulerConfig {
	return SchedulerConfig{MaxParallelism: 2, MaxPixelsPerWorker: 4096, QueueBudgetTiles: 4}
}

type QueueWaitRecord struct {
	TileID   int    `json:"tile_id"`
	Reason   string `json:"reason"`
	WaitedMS int64  `json:"waited_ms"`
}

type SchedulerTrace struct {
	Config     SchedulerConfig   `json:"config"`
	QueueWaits []QueueWaitRecord `json:"queue_waits"`
	WorkerIDs  []int             `json:"worker_ids_used"`
	mu         sync.Mutex
}

func (t *SchedulerTrace) recordWait(r QueueWaitRecord) {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.QueueWaits = append(t.QueueWaits, r)
}

func (t *SchedulerTrace) recordWorker(id int) {
	t.mu.Lock()
	defer t.mu.Unlock()
	for _, existing := range t.WorkerIDs {
		if existing == id {
			return
		}
	}
	t.WorkerIDs = append(t.WorkerIDs, id)
}

type TileResult[T any] struct {
	Tile       Tile      `json:"tile"`
	Value      T         `json:"-"`
	Err        error     `json:"-"`
	ErrorCode  ErrorCode `json:"error_code,omitempty"`
	Attempts   int       `json:"attempts"`
	WorkerID   int       `json:"worker_id"`
	DurationMS int64     `json:"duration_ms"`
	Status     string    `json:"status"`
}

// RunTileTasks executes independent tile tasks with bounded workers, buffers and queue.
func RunTileTasks[T any](ctx context.Context, tiles []Tile, cfg SchedulerConfig, task func(context.Context, Tile, int) (T, error)) ([]TileResult[T], *SchedulerTrace) {
	if cfg.MaxParallelism <= 0 {
		cfg.MaxParallelism = minInt(2, runtime.GOMAXPROCS(0))
		if cfg.MaxParallelism < 1 {
			cfg.MaxParallelism = 1
		}
	}
	if cfg.MaxPixelsPerWorker <= 0 {
		cfg.MaxPixelsPerWorker = 4096
	}
	if cfg.QueueBudgetTiles <= 0 {
		cfg.QueueBudgetTiles = maxInt(1, len(tiles))
	}
	trace := &SchedulerTrace{Config: cfg}
	results := make([]TileResult[T], len(tiles))
	jobs := make(chan int, cfg.QueueBudgetTiles)
	var wg sync.WaitGroup

	for workerID := 1; workerID <= cfg.MaxParallelism; workerID++ {
		wg.Add(1)
		go func(wid int) {
			defer wg.Done()
			for idx := range jobs {
				tile := tiles[idx]
				trace.recordWorker(wid)
				start := time.Now()
				result := TileResult[T]{Tile: tile, WorkerID: wid, Attempts: 1}
				pixels := tile.Rect.Dx() * tile.Rect.Dy()
				if pixels > cfg.MaxPixelsPerWorker {
					err := paramError("scheduler.max_pixels_per_worker", fmt.Sprintf("<=%d pixels/tile", cfg.MaxPixelsPerWorker), "tile %d needs %d pixel buffer", tile.ID, pixels)
					result.Err = err
					result.ErrorCode = CodePARA
					result.Status = "failed"
				} else {
					v, err := task(ctx, tile, wid)
					result.Value = v
					result.Err = err
					if err != nil {
						result.Status = "failed"
						if qe, ok := AsError(err); ok {
							result.ErrorCode = qe.Code
						}
					} else {
						result.Status = "ok"
					}
				}
				result.DurationMS = time.Since(start).Milliseconds()
				results[idx] = result
			}
		}(workerID)
	}

	for idx := range tiles {
		select {
		case <-ctx.Done():
			close(jobs)
			wg.Wait()
			for j := idx; j < len(tiles); j++ {
				if results[j].Status == "" {
					results[j] = TileResult[T]{Tile: tiles[j], Status: "cancelled", Err: ctx.Err()}
				}
			}
			return results, trace
		default:
		}

		waitStart := time.Now()
		select {
		case jobs <- idx:
			if waited := time.Since(waitStart); waited > 2*time.Millisecond {
				trace.recordWait(QueueWaitRecord{TileID: tiles[idx].ID, Reason: "worker_pool_or_pixel_buffer_exhausted", WaitedMS: waited.Milliseconds()})
			}
		default:
			trace.recordWait(QueueWaitRecord{TileID: tiles[idx].ID, Reason: "queue_budget_exhausted", WaitedMS: 0})
			jobs <- idx
			waited := time.Since(waitStart).Milliseconds()
			if waited < 0 {
				waited = 0
			}
			trace.mu.Lock()
			for k := range trace.QueueWaits {
				if trace.QueueWaits[k].TileID == tiles[idx].ID && trace.QueueWaits[k].WaitedMS == 0 {
					trace.QueueWaits[k].WaitedMS = waited
					break
				}
			}
			trace.mu.Unlock()
		}
	}
	close(jobs)
	wg.Wait()
	return results, trace
}

// RetryTile reruns one failed tile without affecting other tile results.
func RetryTile[T any](ctx context.Context, tile Tile, cfg SchedulerConfig, task func(context.Context, Tile, int) (T, error)) TileResult[T] {
	results, _ := RunTileTasks(ctx, []Tile{tile}, cfg, task)
	if len(results) == 0 {
		return TileResult[T]{Tile: tile, Status: "failed", Err: fmt.Errorf("retry did not run")}
	}
	r := results[0]
	r.Attempts = 2
	return r
}
