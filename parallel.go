package lightprobe

import (
	"context"
	"sync"
	"sync/atomic"
	"time"
)

type PoolConfig struct {
	Workers     int `json:"workers"`
	BufferLimit int `json:"buffer_limit"`
	QueueBudget int `json:"queue_budget"`
}

type QueueWait struct {
	ProbeID string  `json:"probe_id"`
	Reason  string  `json:"reason"`
	WaitNS  int64   `json:"wait_ns"`
	WaitMS  float64 `json:"wait_ms"`
}

type QueueStats struct {
	Workers            int         `json:"workers"`
	BufferLimit        int         `json:"buffer_limit"`
	QueueBudget        int         `json:"queue_budget"`
	ProbeCount         int         `json:"probe_count"`
	Submitted          int         `json:"submitted"`
	Completed          int         `json:"completed"`
	PeakActiveWorkers  int32       `json:"peak_active_workers"`
	PeakQueued         int         `json:"peak_queued"`
	PeakBufferObserved int         `json:"peak_buffer_observed"`
	WaitCount          int         `json:"wait_count"`
	TotalWaitNS        int64       `json:"total_wait_ns"`
	TotalWaitMS        float64     `json:"total_wait_ms"`
	Waits              []QueueWait `json:"waits"`
}

type poolBatch struct {
	worker int
	items  []ProbeResult
}

func RunProbePool(probes []Probe, params Params, cfg PoolConfig) (map[string]ProbeResult, QueueStats, error) {
	return RunProbePoolContext(context.Background(), probes, params, cfg)
}

func RunProbePoolContext(ctx context.Context, probes []Probe, params Params, cfg PoolConfig) (map[string]ProbeResult, QueueStats, error) {
	if cfg.Workers <= 0 {
		cfg.Workers = params.Workers
	}
	if cfg.BufferLimit <= 0 {
		cfg.BufferLimit = params.BufferLimit
	}
	if cfg.QueueBudget <= 0 {
		cfg.QueueBudget = params.QueueBudget
	}
	if cfg.Workers <= 0 || cfg.BufferLimit <= 0 || cfg.QueueBudget <= 0 {
		return nil, QueueStats{}, domainError(EVal, "pool", "budget", "pool limits must be positive")
	}

	stats := QueueStats{
		Workers: cfg.Workers, BufferLimit: cfg.BufferLimit, QueueBudget: cfg.QueueBudget,
		ProbeCount: len(probes),
	}
	jobs := make(chan int, cfg.QueueBudget)
	batches := make(chan poolBatch, cfg.Workers*2+1)
	results := make([]ProbeResult, len(probes))

	var (
		wg        sync.WaitGroup
		statsMu   sync.Mutex
		active    int32
		firstErr  error
		errOnce   sync.Once
		completed int32
	)

	recordWait := func(w QueueWait) {
		statsMu.Lock()
		stats.Waits = append(stats.Waits, w)
		stats.WaitCount++
		stats.TotalWaitNS += w.WaitNS
		stats.TotalWaitMS = float64(stats.TotalWaitNS) / 1e6
		statsMu.Unlock()
	}
	setPeakBuffer := func(n int) {
		statsMu.Lock()
		if n > stats.PeakBufferObserved {
			stats.PeakBufferObserved = n
		}
		statsMu.Unlock()
	}

	for w := 0; w < cfg.Workers; w++ {
		wg.Add(1)
		go func(workerID int) {
			defer wg.Done()
			buffer := make([]ProbeResult, 0, cfg.BufferLimit)
			flush := func() {
				if len(buffer) == 0 {
					return
				}
				setPeakBuffer(len(buffer))
				copyItems := append([]ProbeResult(nil), buffer...)
				select {
				case batches <- poolBatch{worker: workerID, items: copyItems}:
				case <-ctx.Done():
					return
				}
				buffer = buffer[:0]
			}
			defer flush()

			for idx := range jobs {
				if ctx.Err() != nil {
					return
				}
				cur := atomic.AddInt32(&active, 1)
				for {
					peak := atomic.LoadInt32(&stats.PeakActiveWorkers)
					if cur <= peak || atomic.CompareAndSwapInt32(&stats.PeakActiveWorkers, peak, cur) {
						break
					}
				}
				res, err := ReconstructProbe(probes[idx], params)
				atomic.AddInt32(&active, -1)
				if err != nil {
					errOnce.Do(func() { firstErr = err })
					continue
				}
				res.Index = idx
				buffer = append(buffer, res)
				if len(buffer) >= cfg.BufferLimit {
					flush()
				}
				atomic.AddInt32(&completed, 1)
			}
		}(w)
	}

	collectorDone := make(chan struct{})
	go func() {
		for batch := range batches {
			for _, item := range batch.items {
				results[item.Index] = item
			}
		}
		close(collectorDone)
	}()

	submitted := 0
submit:
	for i := range probes {
		select {
		case <-ctx.Done():
			break submit
		default:
		}
		select {
		case jobs <- i:
		default:
			start := time.Now()
			select {
			case jobs <- i:
				d := time.Since(start)
				recordWait(QueueWait{
					ProbeID: probes[i].ID, Reason: "pending_queue_budget_exhausted",
					WaitNS: d.Nanoseconds(), WaitMS: float64(d.Nanoseconds()) / 1e6,
				})
			case <-ctx.Done():
				break submit
			}
		}
		submitted++
		statsMu.Lock()
		stats.Submitted = submitted
		if ql := len(jobs); ql > stats.PeakQueued {
			stats.PeakQueued = ql
		}
		statsMu.Unlock()
	}
	close(jobs)
	wg.Wait()
	close(batches)
	<-collectorDone

	stats.Completed = int(atomic.LoadInt32(&completed))
	stats.TotalWaitMS = float64(stats.TotalWaitNS) / 1e6
	if firstErr != nil {
		return nil, stats, firstErr
	}
	if ctx.Err() != nil {
		return nil, stats, ctx.Err()
	}

	out := make(map[string]ProbeResult, len(probes))
	for i, r := range results {
		if r.ProbeID == "" {
			err := domainError(EVal, probes[i].ID, "pool_result", "probe was not reconstructed")
			return nil, stats, err
		}
		out[r.ProbeID] = r
	}
	return out, stats, nil
}
