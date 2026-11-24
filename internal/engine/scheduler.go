package engine

import (
	"fmt"
	"runtime/debug"
	"sync"
	"time"

	"terrainnormalverify/internal/bake"
	"terrainnormalverify/internal/model"
)

type Budget struct {
	MaxParallelism       int   `json:"maxParallelism"`
	PerThreadPixelBuffer int   `json:"perThreadPixelBuffer"`
	NormalMapMemoryBytes int64 `json:"normalMapMemoryBytes"`
}

func DefaultBudget() Budget {
	return Budget{
		MaxParallelism:       4,
		PerThreadPixelBuffer: 4096,
		NormalMapMemoryBytes: 64 << 20,
	}
}

func (b Budget) Validate() error {
	if b.MaxParallelism <= 0 {
		return model.NewError(model.ErrBudget, -1, "maxParallelism", "must be positive")
	}
	if b.PerThreadPixelBuffer <= 0 {
		return model.NewError(model.ErrBudget, -1, "perThreadPixelBuffer", "must be positive")
	}
	if b.NormalMapMemoryBytes <= 0 {
		return model.NewError(model.ErrBudget, -1, "normalMapMemoryBytes", "must be positive")
	}
	return nil
}

func EstimateResultBytes(pixels int) int64 {
	return int64(pixels)*128 + 4096
}

type QueueRecord struct {
	Sequence       int    `json:"sequence"`
	BlockID        int    `json:"blockId"`
	Reason         string `json:"reason"`
	EstimatedBytes int64  `json:"estimatedBytes"`
}

type scheduledResult struct {
	ID       int
	Result   *bake.BlockResult
	Err      error
	Duration time.Duration
}

type ScheduleReport struct {
	Results     map[int]*bake.BlockResult
	Failures    map[int]error
	Queue       []QueueRecord
	Durations   map[int]time.Duration
	PeakMemory  int64
	WorkerCount int
}

type memoryPool struct {
	limit int64
	used  int64
	peak  int64
	cond  *sync.Cond
}

func (p *memoryPool) peakValue() int64 {
	p.cond.L.Lock()
	defer p.cond.L.Unlock()
	return p.peak
}

func newMemoryPool(limit int64) *memoryPool {
	pool := &memoryPool{limit: limit}
	pool.cond = sync.NewCond(&sync.Mutex{})
	return pool
}

func (p *memoryPool) acquire(bytes int64) {
	p.cond.L.Lock()
	for p.used+bytes > p.limit {
		p.cond.Wait()
	}
	p.used += bytes
	if p.used > p.peak {
		p.peak = p.used
	}
	p.cond.L.Unlock()
}

func (p *memoryPool) release(bytes int64) {
	p.cond.L.Lock()
	p.used -= bytes
	p.cond.Broadcast()
	p.cond.L.Unlock()
}

func Schedule(project *model.Project, blockIDs []int, budget Budget, faultBlock int) *ScheduleReport {
	report := &ScheduleReport{
		Results:   make(map[int]*bake.BlockResult),
		Failures:  make(map[int]error),
		Durations: make(map[int]time.Duration),
	}
	if err := budget.Validate(); err != nil {
		for _, id := range blockIDs {
			report.Failures[id] = err
		}
		return report
	}
	workers := budget.MaxParallelism
	if workers > len(blockIDs) {
		workers = len(blockIDs)
	}
	report.WorkerCount = workers
	if len(blockIDs) == 0 {
		return report
	}
	cumulative := int64(0)
	for sequence, id := range blockIDs {
		block, ok := project.Block(id)
		if !ok {
			report.Failures[id] = model.NewError(model.ErrSchema, id, "block", "block does not exist")
			continue
		}
		estimate := EstimateResultBytes(block.PixelCount())
		reason := ""
		if sequence >= budget.MaxParallelism {
			reason = "max_parallelism"
		} else if cumulative+estimate > budget.NormalMapMemoryBytes {
			reason = "normal_map_memory_budget"
		}
		if block.PixelCount() > budget.PerThreadPixelBuffer {
			if reason == "" {
				reason = "per_thread_buffer_requires_tiling"
			}
		}
		if reason != "" {
			report.Queue = append(report.Queue, QueueRecord{
				Sequence:       sequence,
				BlockID:        id,
				Reason:         reason,
				EstimatedBytes: estimate,
			})
		}
		cumulative += estimate
	}

	type job struct {
		sequence int
		id       int
	}
	jobs := make(chan job, len(blockIDs))
	var wg sync.WaitGroup
	var resultMu sync.Mutex
	pool := newMemoryPool(budget.NormalMapMemoryBytes)
	for i := 0; i < workers; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for item := range jobs {
				block, ok := project.Block(item.id)
				if !ok {
					resultMu.Lock()
					report.Failures[item.id] = model.NewError(model.ErrSchema, item.id, "block", "block does not exist")
					resultMu.Unlock()
					continue
				}
				need := EstimateResultBytes(block.PixelCount())
				if need > budget.NormalMapMemoryBytes {
					e := model.NewError(model.ErrBudget, item.id, "normalMapMemoryBytes", "single block result exceeds normal-map memory budget")
					e.Value = need
					e.Want = budget.NormalMapMemoryBytes
					resultMu.Lock()
					report.Failures[item.id] = e
					resultMu.Unlock()
					continue
				}
				pool.acquire(need)
				started := time.Now()
				result, err := bakeSafely(*block, project.Layers, project.Parameters, budget.PerThreadPixelBuffer, item.id, faultBlock)
				duration := time.Since(started)
				pool.release(need)
				if err != nil {
					resultMu.Lock()
					report.Failures[item.id] = err
					resultMu.Unlock()
					continue
				}
				resultMu.Lock()
				report.Results[item.id] = result
				report.Durations[item.id] = duration
				resultMu.Unlock()
			}
		}()
	}
	for sequence, id := range blockIDs {
		jobs <- job{sequence: sequence, id: id}
	}
	close(jobs)
	wg.Wait()
	report.PeakMemory = pool.peakValue()
	return report
}

func bakeSafely(block model.Block, layers []model.MaterialLayer, params model.BakeParameters, bufferPixels, id, faultBlock int) (result *bake.BlockResult, err error) {
	defer func() {
		if recovered := recover(); recovered != nil {
			e := model.NewError(model.ErrBlock, id, "bake", fmt.Sprintf("block panic: %v", recovered))
			e.Value = string(debug.Stack())
			err = e
		}
	}()
	if id == faultBlock {
		return nil, model.NewError(model.ErrBlock, id, "bake", "injected block failure")
	}
	return bake.BakeBlock(block, layers, params, bufferPixels)
}
