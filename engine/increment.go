package engine

import (
	"context"
	"fmt"

	"economycore/model"
)

// RunWindow settles one complete window. It is used by the checkpoint runner.
func (e *Engine) RunWindow(ctx context.Context, window int) (WindowResult, error) {
	if window < 0 || window >= e.model.WindowCount {
		return WindowResult{}, model.NewError(model.EWindow, "/window", "window out of range").WithWindow(window)
	}
	expectedStart := int64(window * e.model.WindowCycles)
	if e.state.NextCycle != expectedStart {
		return WindowResult{}, model.NewError(model.EWindow, "/window", fmt.Sprintf("window requires next cycle %d, state is at %d", expectedStart, e.state.NextCycle)).WithWindow(window)
	}
	result := WindowResult{
		Window:       window,
		StartCycle:   expectedStart,
		State:        model.RuntimeState{},
		IntentOffset: e.state.IntentIndex,
	}
	stagedCache := cloneTransitionMap(e.cache)
	reuseStats := newAggregateStats()
	for cycle := expectedStart; cycle < expectedStart+int64(e.model.WindowCycles); cycle++ {
		if err := ctx.Err(); err != nil {
			return WindowResult{}, model.NewError(model.ECancel, "/context", err.Error()).WithWindow(window).WithCause(err)
		}
		snapshot, cycleCache, stats, err := e.runCycle(ctx)
		if err != nil {
			return WindowResult{}, err
		}
		mergeCycleCache(stagedCache, cycleCache)
		reuseStats.add(stats)
		result.Cycles = append(result.Cycles, snapshot)
		result.ReusedCycles += stats.Reused
		result.RecomputedCycles += stats.Recomputed
	}
	e.cache = stagedCache
	result.State = e.State()
	result.IntentOffset = e.state.IntentIndex
	e.lastReasons = reuseStats.reasons()
	e.forcedInvalid = make(map[string][]string)
	return result, nil
}

// RunAll settles from the current state to the end of the project.
func (e *Engine) RunAll(ctx context.Context) (*RunResult, error) {
	result := &RunResult{
		OverflowTotals: make(map[string]int64),
		FinalState:     model.RuntimeState{},
	}
	stagedCache := cloneTransitionMap(e.cache)
	reuseStats := newAggregateStats()
	for e.state.NextCycle < e.model.TotalCycles() {
		if err := ctx.Err(); err != nil {
			result.FinalState = e.State()
			result.IntentOffset = e.state.IntentIndex
			result.Complete = false
			return result, model.NewError(model.ECancel, "/context", err.Error()).WithCause(err)
		}
		snapshot, cycleCache, stats, err := e.runCycle(ctx)
		if err != nil {
			result.FinalState = e.State()
			result.IntentOffset = e.state.IntentIndex
			result.Complete = false
			return result, err
		}
		mergeCycleCache(stagedCache, cycleCache)
		reuseStats.add(stats)
		result.Cycles = append(result.Cycles, snapshot)
		result.ReusedCycles += stats.Reused
		result.RecomputedCycles += stats.Recomputed
	}
	e.cache = stagedCache
	result.FinalState = e.State()
	result.OverflowTotals = cloneMap(e.state.OverflowTotals)
	result.IntentOffset = e.state.IntentIndex
	result.Complete = e.state.NextCycle == e.model.TotalCycles()
	result.Reuse = e.reuseReport(reuseStats)
	e.lastReasons = reuseStats.reasons()
	e.forcedInvalid = make(map[string][]string)
	return result, nil
}

// RunFull clears the cache and performs a from-empty full settlement.
func (e *Engine) RunFull(ctx context.Context) (*RunResult, error) {
	if err := e.Reset(); err != nil {
		return nil, err
	}
	e.cache = make(map[cacheKey]nodeTransition)
	e.forcedInvalid = make(map[string][]string)
	return e.RunAll(ctx)
}

// RunIncremental resets state and replays cached transitions where their
// dependency signature and input preconditions still match.
func (e *Engine) RunIncremental(ctx context.Context) (*RunResult, error) {
	if err := e.Reset(); err != nil {
		return nil, err
	}
	return e.RunAll(ctx)
}

func (e *Engine) reuseReport(stats aggregateStats) []ReuseStatus {
	out := make([]ReuseStatus, 0, len(e.model.Nodes))
	for _, node := range e.model.Nodes {
		list := stats.reasonsFor(node.ID)
		reusedCycles := stats.reused[node.ID]
		recomputedCycles := stats.recomputed[node.ID]
		reused := reusedCycles > 0 && recomputedCycles == 0
		status := ReuseStatus{
			NodeID:           node.ID,
			Reused:           reused,
			ReusedCycles:     reusedCycles,
			RecomputedCycles: recomputedCycles,
			Reasons:          append([]string(nil), list...),
		}
		out = append(out, status)
	}
	return out
}

func cloneTransitionMap(in map[cacheKey]nodeTransition) map[cacheKey]nodeTransition {
	out := make(map[cacheKey]nodeTransition, len(in))
	for key, transition := range in {
		out[key] = cloneTransition(transition)
	}
	return out
}

func mergeCycleCache(dst, src map[cacheKey]nodeTransition) {
	for key, transition := range src {
		dst[key] = cloneTransition(transition)
	}
}

type aggregateStats struct {
	reused     map[string]int
	recomputed map[string]int
	reasonMap  map[string][]string
}

func newAggregateStats() aggregateStats {
	return aggregateStats{
		reused:     make(map[string]int),
		recomputed: make(map[string]int),
		reasonMap:  make(map[string][]string),
	}
}

func (a *aggregateStats) add(cycle cycleStats) {
	for nodeID := range cycle.NodeReused {
		a.reused[nodeID]++
	}
	for nodeID := range cycle.NodeRecomputed {
		a.recomputed[nodeID]++
	}
	for nodeID, reasons := range cycle.Reasons {
		for _, reason := range reasons {
			a.reasonMap[nodeID] = appendUnique(a.reasonMap[nodeID], reason)
		}
	}
}

func (a *aggregateStats) reasonsFor(nodeID string) []string {
	return a.reasonMap[nodeID]
}

func (a *aggregateStats) reasons() map[string][]string {
	out := make(map[string][]string, len(a.reasonMap))
	for key, value := range a.reasonMap {
		out[key] = append([]string(nil), value...)
	}
	return out
}
