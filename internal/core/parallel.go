package core

import (
	"context"
	"sort"
	"sync"
)

type ParallelOptions struct {
	WindowSize       int64
	Workers          int
	BufferPerWorker  int
	StopAfterWindows int
	SampleTicks      []int64
}

type WindowPlan struct {
	Index             int   `json:"index"`
	StartTick         int64 `json:"start_tick"`
	EndTick           int64 `json:"end_tick"`
	StartIntentOffset int   `json:"start_intent_offset"`
	EndIntentOffset   int   `json:"end_intent_offset"`
	IntentCount       int   `json:"intent_count"`
}

type PlanStats struct {
	Workers         int `json:"workers"`
	BufferPerWorker int `json:"buffer_per_worker"`
	Planned         int `json:"planned"`
}

// RunWindowedParallel uses the same deterministic, serial state fold as
// RunWindowed. The worker pool prepares immutable window plans ahead of the
// fold: timestamp slicing, intent validation and checkpoint metadata. Plans are
// carried in a bounded channel, so a full per-worker buffer blocks producers
// instead of dropping windows. Cross-window state merge remains ordered.
func RunWindowedParallel(
	ctx context.Context,
	scene *Scenario,
	sink CheckpointStore,
	opts ParallelOptions,
) (*WindowRunResult, PlanStats, error) {
	if opts.Workers < 1 || opts.Workers > 32 {
		return nil, PlanStats{}, NewError(CodeParam, "worker count must be in 1..32").WithField("workers")
	}
	if opts.BufferPerWorker < 1 || opts.BufferPerWorker > 1024 {
		return nil, PlanStats{}, NewError(CodeParam, "buffer per worker must be in 1..1024").
			WithField("buffer_per_worker")
	}
	if opts.WindowSize <= 0 {
		return nil, PlanStats{}, NewError(CodeParam, "window size must be positive").WithField("window_size")
	}
	windowCount := 0
	if scene.FinalTick > 0 {
		windowCount = int((scene.FinalTick + opts.WindowSize - 1) / opts.WindowSize)
	}
	stats := PlanStats{
		Workers:         opts.Workers,
		BufferPerWorker: opts.BufferPerWorker,
	}

	planCtx, cancel := context.WithCancel(ctx)
	defer cancel()
	jobs := make(chan int)
	results := make(chan struct {
		plan WindowPlan
		err  error
	}, opts.Workers*opts.BufferPerWorker)
	var wg sync.WaitGroup
	for w := 0; w < opts.Workers; w++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for index := range jobs {
				plan, err := BuildWindowPlan(scene, index, opts.WindowSize)
				select {
				case results <- struct {
					plan WindowPlan
					err  error
				}{plan, err}:
				case <-planCtx.Done():
					return
				}
			}
		}()
	}
	go func() {
		defer close(jobs)
		for i := 0; i < windowCount; i++ {
			select {
			case jobs <- i:
			case <-planCtx.Done():
				return
			}
		}
	}()
	go func() {
		wg.Wait()
		close(results)
	}()

	result, runErr := RunWindowed(ctx, scene, sink, WindowOptions{
		WindowSize:       opts.WindowSize,
		StopAfterWindows: opts.StopAfterWindows,
		SampleTicks:      opts.SampleTicks,
	})
	if runErr != nil {
		cancel()
		// Drain until workers observe cancellation.
		for range results {
		}
		return nil, stats, runErr
	}

	plans := make(map[int]WindowPlan, windowCount)
	var planErr error
	for item := range results {
		stats.Planned++
		if item.err != nil && planErr == nil {
			planErr = item.err
		}
		if item.err == nil {
			plans[item.plan.Index] = item.plan
		}
	}
	if planErr != nil {
		return nil, stats, planErr
	}
	for _, w := range result.Windows {
		plan, ok := plans[w.Index]
		if !ok {
			return nil, stats, NewError(CodeState, "parallel planner did not produce a committed window").
				WithValues(w.Index, nil)
		}
		if plan.StartTick != w.StartTick || plan.EndTick != w.EndTick ||
			plan.StartIntentOffset != w.StartIntentOffset ||
			plan.EndIntentOffset != w.EndIntentOffset {
			return nil, stats, NewError(CodeDiverge, "parallel window plan diverged from serial settlement").
				WithValues(plan, w)
		}
	}
	// Validate that committed windows are still strictly ordered.
	for i := 1; i < len(result.Windows); i++ {
		if result.Windows[i-1].Index+1 != result.Windows[i].Index {
			return nil, stats, NewError(CodeDiverge, "window commit order is not contiguous").
				WithValues(result.Windows[i-1].Index, result.Windows[i].Index)
		}
	}
	return result, stats, nil
}

func BuildWindowPlan(scene *Scenario, index int, windowSize int64) (WindowPlan, error) {
	if index < 0 || windowSize <= 0 {
		return WindowPlan{}, NewError(CodeParam, "invalid window plan request")
	}
	start := int64(index) * windowSize
	if start >= scene.FinalTick {
		return WindowPlan{}, NewError(CodeParam, "window starts after final_tick").
			WithValues(start, scene.FinalTick)
	}
	end := start + windowSize
	if end > scene.FinalTick {
		end = scene.FinalTick
	}
	startOffset := sort.Search(len(scene.Intents), func(i int) bool {
		return scene.Intents[i].Timestamp >= start
	})
	endOffset := sort.Search(len(scene.Intents), func(i int) bool {
		return scene.Intents[i].Timestamp >= end
	})
	nodeMap := scene.NodeMap()
	for i := startOffset; i < endOffset; i++ {
		if _, ok := nodeMap[scene.Intents[i].NodeID]; !ok {
			return WindowPlan{}, NewError(CodeSchema, "window intent targets an unknown node").
				ForNode(scene.Intents[i].NodeID).ForIntent(scene.Intents[i].Seq, i)
		}
	}
	return WindowPlan{
		Index:             index,
		StartTick:         start,
		EndTick:           end,
		StartIntentOffset: startOffset,
		EndIntentOffset:   endOffset,
		IntentCount:       endOffset - startOffset,
	}, nil
}
