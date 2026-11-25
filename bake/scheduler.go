package bake

import (
	"context"
	"sort"
	"sync"
	"time"

	"terrainbake/geometry"
	"terrainbake/model"
)

type unitTask struct {
	ref         UnitRef
	sun         model.SunSample
	previousKey string
}

type taskResult struct {
	unit *BakedUnit
	err  error
}

func runBakeWave(ctx context.Context, terrain *geometry.Terrain, tasks []unitTask, opts ScheduleOptions) (map[string]*BakedUnit, ScheduleStats, error) {
	stats := ScheduleStats{
		MaxParallelism:  opts.MaxParallelism,
		VertexBufferCap: opts.VertexBufferCap,
		QueueBudget:     opts.QueueBudget,
	}
	out := make(map[string]*BakedUnit, len(tasks))
	if len(tasks) == 0 {
		return out, stats, nil
	}
	if err := opts.Validate(); err != nil {
		return nil, stats, err
	}
	workers := opts.MaxParallelism
	if workers > len(tasks) {
		workers = len(tasks)
	}
	stats.Workers = workers

	taskCh := make(chan unitTask, opts.QueueBudget)
	resultCh := make(chan taskResult, len(tasks))
	var wg sync.WaitGroup
	for i := 0; i < workers; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for task := range taskCh {
				if err := ctx.Err(); err != nil {
					resultCh <- taskResult{err: cancelError(err, task.ref)}
					continue
				}
				chunk, ok := terrain.Chunk(task.ref.ChunkID)
				if !ok {
					resultCh <- taskResult{err: model.NewError(model.CodeValue, "unknown chunk %q", task.ref.ChunkID).WithChunk(task.ref.ChunkID)}
					continue
				}
				unit, err := BakeUnit(terrain, chunk, task.sun, task.previousKey, opts.VertexBufferCap)
				resultCh <- taskResult{unit: unit, err: err}
			}
		}()
	}

	canceled := false
	for _, task := range tasks {
		if ctx.Err() != nil {
			canceled = true
			break
		}
		select {
		case taskCh <- task:
			stats.Enqueued++
		default:
			started := time.Now()
			select {
			case taskCh <- task:
				waited := time.Since(started)
				stats.Enqueued++
				stats.QueueEvents = append(stats.QueueEvents, QueueEvent{
					UnitID: UnitID(task.ref.ChunkID, task.ref.Time), Reason: "queue_budget_exhausted", WaitNano: waited.Nanoseconds(),
				})
			case <-ctx.Done():
				canceled = true
			}
			if canceled {
				break
			}
		}
	}
	close(taskCh)
	wg.Wait()
	close(resultCh)

	for result := range resultCh {
		if result.err != nil {
			return nil, stats, result.err
		}
		if result.unit == nil {
			continue
		}
		out[UnitID(result.unit.ChunkID, result.unit.Time)] = result.unit
		stats.BufferBatches += result.unit.BufferBatches
	}
	if canceled {
		return nil, stats, model.NewError(model.CodeCancel, "bake canceled while scheduling chunks").WithField("schedule")
	}
	return out, stats, nil
}

func cancelError(err error, ref UnitRef) error {
	return model.NewError(model.CodeCancel, "bake canceled: %v", err).
		WithChunk(ref.ChunkID).WithTime(ref.Time)
}

func mergeStats(total *ScheduleStats, wave ScheduleStats) {
	if wave.MaxParallelism > total.MaxParallelism {
		total.MaxParallelism = wave.MaxParallelism
	}
	if total.VertexBufferCap == 0 {
		total.VertexBufferCap = wave.VertexBufferCap
	}
	if wave.QueueBudget < total.QueueBudget || total.QueueBudget == 0 {
		total.QueueBudget = wave.QueueBudget
	}
	if wave.Workers > total.Workers {
		total.Workers = wave.Workers
	}
	total.Enqueued += wave.Enqueued
	total.BufferBatches += wave.BufferBatches
	total.QueueEvents = append(total.QueueEvents, wave.QueueEvents...)
}

func sortRefs(refs []UnitRef) {
	sort.Slice(refs, func(i, j int) bool {
		if refs[i].Time != refs[j].Time {
			return refs[i].Time < refs[j].Time
		}
		return refs[i].ChunkID < refs[j].ChunkID
	})
}
