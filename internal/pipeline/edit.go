package pipeline

import (
	"bytes"
	"context"
	"fmt"
	"sort"

	"qualitybench/internal/checkpoint"
	"qualitybench/internal/core"
	"qualitybench/internal/model"
	"qualitybench/internal/ops"
	"qualitybench/internal/quality"
)

func (e *Engine) ApplyEdit(ctx context.Context, edit model.Edit) (*EditResult, error) {
	if !e.ran || len(e.frames) == 0 || e.frames[len(e.chain.Steps)] == nil {
		return nil, core.StateError("pending", "completed", 0, nil, "engine must complete before editing")
	}
	if edit.ImageID != "" && edit.ImageID != e.opts.ImageID {
		return nil, core.Range("image_id", "edit targets %q, engine owns %q", edit.ImageID, e.opts.ImageID)
	}
	oldChain := e.chain.Clone()
	newChain := oldChain.Clone()
	earliest := 0
	var direct map[int]bool
	var forced map[int]map[int]bool
	reason := "step_parameter_changed"
	switch edit.Op {
	case "set_strength":
		if edit.Step < 1 || edit.Step > len(newChain.Steps) {
			return nil, core.Range("step", "step %d outside chain", edit.Step)
		}
		if edit.Strength == nil {
			return nil, core.Schema("strength", "strength is required")
		}
		oldStep := newChain.Steps[edit.Step-1]
		newStep := oldStep
		newStep.Strength = *edit.Strength
		newChain.Steps[edit.Step-1] = newStep
		earliest = edit.Step
		direct = intersectionBlocks(e.grid, oldStep.RegionRect(e.baseline.Width, e.baseline.Height),
			newStep.RegionRect(e.baseline.Width, e.baseline.Height))
		forced = map[int]map[int]bool{earliest: direct}
	case "set_region":
		if edit.Step < 1 || edit.Step > len(newChain.Steps) {
			return nil, core.Range("step", "step %d outside chain", edit.Step)
		}
		oldStep := newChain.Steps[edit.Step-1]
		newStep := oldStep
		if edit.Region == nil {
			newStep.Region = nil
		} else {
			r := *edit.Region
			newStep.Region = &r
		}
		newChain.Steps[edit.Step-1] = newStep
		earliest = edit.Step
		direct = intersectionBlocks(e.grid, oldStep.RegionRect(e.baseline.Width, e.baseline.Height),
			newStep.RegionRect(e.baseline.Width, e.baseline.Height))
		forced = map[int]map[int]bool{earliest: direct}
	case "move_step":
		n := len(newChain.Steps)
		if edit.From < 1 || edit.From > n || edit.To < 1 || edit.To > n {
			return nil, core.Range("from", "step positions must be in [1,%d]", n)
		}
		if edit.From == edit.To {
			return e.noopEdit()
		}
		step := newChain.Steps[edit.From-1]
		newChain.Steps = append(newChain.Steps[:edit.From-1], newChain.Steps[edit.From:]...)
		insert := edit.To - 1
		if insert > len(newChain.Steps) {
			insert = len(newChain.Steps)
		}
		newChain.Steps = append(newChain.Steps, model.Step{})
		copy(newChain.Steps[insert+1:], newChain.Steps[insert:])
		newChain.Steps[insert] = step
		earliest = minInt(edit.From, edit.To)
		direct = make(map[int]bool)
		maxPos := maxInt(edit.From, edit.To)
		for i := earliest - 1; i < maxPos && i < len(oldChain.Steps); i++ {
			markIntersections(direct, e.grid, oldChain.Steps[i].RegionRect(e.baseline.Width, e.baseline.Height))
		}
		for i := earliest - 1; i < maxPos && i < len(newChain.Steps); i++ {
			markIntersections(direct, e.grid, newChain.Steps[i].RegionRect(e.baseline.Width, e.baseline.Height))
		}
		forced = make(map[int]map[int]bool)
		for pos := earliest; pos <= maxPos; pos++ {
			forced[pos] = direct
		}
		reason = "step_order_changed"
	case "rollback":
		return e.Rollback(ctx, edit.Version)
	default:
		return nil, core.Para(edit.Step, "op", "unknown edit op %q", edit.Op)
	}
	if err := model.ValidateSteps(newChain.Steps, e.baseline.Width, e.baseline.Height); err != nil {
		return nil, err
	}
	if model.ChainFingerprint(newChain, e.opts.AlgorithmVersion) ==
		model.ChainFingerprint(oldChain, e.opts.AlgorithmVersion) {
		return e.noopEdit()
	}
	return e.transformChain(ctx, newChain, earliest, forced, reason)
}

func (e *Engine) Rollback(ctx context.Context, versionID string) (*EditResult, error) {
	if !e.ran || len(e.frames) == 0 || e.frames[len(e.chain.Steps)] == nil {
		return nil, core.StateError("pending", "completed", 0, nil, "engine must complete before rollback")
	}
	var target *Version
	for i := range e.versions {
		if e.versions[i].ID == versionID {
			target = &e.versions[i]
			break
		}
	}
	if target == nil {
		return nil, core.StateError("unknown", "rollback", 0, nil, fmt.Sprintf("version %q not found", versionID))
	}
	if model.ChainFingerprint(target.Chain, e.opts.AlgorithmVersion) ==
		model.ChainFingerprint(e.chain, e.opts.AlgorithmVersion) {
		return e.noopEdit()
	}
	all := make(map[int]bool, len(e.grid))
	for i := range e.grid {
		all[i] = true
	}
	forced := make(map[int]map[int]bool)
	for t := 1; t <= len(target.Chain.Steps); t++ {
		forced[t] = all
	}
	return e.transformChain(ctx, target.Chain.Clone(), 1, forced, "rollback_cache_replay:"+versionID)
}

func (e *Engine) RetryBlock(ctx context.Context, step, blockIndex int) (*EditResult, error) {
	if step < 1 || step > len(e.chain.Steps) || blockIndex < 0 || blockIndex >= len(e.grid) {
		return nil, core.Range("block", "step or block index outside matrix")
	}
	state := e.BlockState(step, blockIndex)
	if state != core.StateCommitted && state != core.StateFailed {
		return nil, core.StateError(string(state), "retry", step,
			blockPtr(checkpoint.BlockRef(blockIndex, e.grid[blockIndex])),
			"only committed or failed blocks can be retried")
	}
	if state == core.StateFailed {
		if err := e.TryBlockTransition(step, blockIndex, core.StatePending); err != nil {
			return nil, err
		}
	}
	return e.transformChain(ctx, e.chain.Clone(), step, map[int]map[int]bool{step: {blockIndex: true}},
		fmt.Sprintf("block_retry:%d,%d", e.grid[blockIndex].X, e.grid[blockIndex].Y))
}

func (e *Engine) transformChain(ctx context.Context, newChain model.Chain, earliest int,
	forced map[int]map[int]bool, reason string) (*EditResult, error) {
	n := len(e.chain.Steps)
	oldFrames := append([]*core.Frame(nil), e.frames...)
	newFrames := make([]*core.Frame, n+1)
	copy(newFrames, oldFrames)
	newKeys := cloneKeys(e.keys)
	newInputs := cloneKeys(e.inputHashes)
	result := &EditResult{ChainFingerprint: model.ChainFingerprint(newChain, e.opts.AlgorithmVersion)}
	everChanged := make(map[int]bool)
	dirty := make(map[int]bool)
	for t := earliest; t <= n; t++ {
		if err := ctx.Err(); err != nil {
			return nil, core.CanceledError("edit canceled")
		}
		step := newChain.Steps[t-1]
		action := step.RegionRect(e.baseline.Width, e.baseline.Height)
		prevNew := newFrames[t-1]
		if prevNew == nil || oldFrames[t] == nil {
			return nil, core.Stage(t, core.BlockRef{Index: 0}, "frame is incomplete")
		}
		frame := oldFrames[t].Clone()
		affected := make(map[int]bool)
		carry := make(map[int]bool)
		nextDirty := make(map[int]bool)
		if t == earliest {
			for b := range forced[t] {
				if action.Intersects(e.grid[b]) {
					affected[b] = true
				} else {
					carry[b] = true
				}
			}
		} else {
			for b := range forced[t] {
				if action.Intersects(e.grid[b]) {
					affected[b] = true
				} else {
					carry[b] = true
				}
			}
			for b := range dirty {
				if action.Intersects(e.grid[b]) {
					affected[b] = true
				} else {
					carry[b] = true
				}
			}
			radius := model.InputSupportRadius(step)
			for b, rect := range e.grid {
				if !action.Intersects(rect) {
					continue
				}
				inputRegion := rect
				if radius > 0 {
					inputRegion = rect.ExpandClamped(radius, e.baseline.Width, e.baseline.Height)
				}
				for d := range dirty {
					if inputRegion.Intersects(e.grid[d]) {
						affected[b] = true
						break
					}
				}
			}
		}
		for b := range carry {
			oldBlock := oldFrames[t].BlockBytes(e.grid[b])
			newBlock := prevNew.BlockBytes(e.grid[b])
			if !bytes.Equal(oldBlock, newBlock) {
				if err := frame.SetBlock(e.grid[b], blockFrame(prevNew, e.grid[b])); err != nil {
					return nil, core.Stage(t, checkpoint.BlockRef(b, e.grid[b]), "%v", err)
				}
			}
			result.Invalidations = append(result.Invalidations, Invalidation{
				Step: t, Block: blockPtr(checkpoint.BlockRef(b, e.grid[b])),
				Reason: "upstream_dirty_passthrough",
			})
		}
		touched := make(map[int]bool)
		for b := range affected {
			touched[b] = true
		}
		for b := range carry {
			touched[b] = true
		}
		if len(touched) > 0 {
			if err := e.prepareEditStep(t, touched); err != nil {
				return nil, err
			}
		}
		processed := make(map[int]bool)
		keys := sortedInts(affected)
		for _, b := range keys {
			processed[b] = true
			rect := e.grid[b]
			upstream := newKeys[t-1][b]
			inputRegion := rect
			radius := model.InputSupportRadius(step)
			if radius > 0 {
				inputRegion = rect.ExpandClamped(radius, e.baseline.Width, e.baseline.Height)
			}
			inputHash := core.HashFrameRegion(prevNew, inputRegion)
			key := CacheKey(e.opts.AlgorithmVersion, t, inputHash, step.CanonicalParams(), upstream)
			var pixels []byte
			hit := false
			if e.opts.ModeUsesCache() {
				if v, ok := e.cache.Get(key); ok && len(v.Pixels) == rect.W*rect.H*e.baseline.Channels {
					pixels = append([]byte(nil), v.Pixels...)
					hit = true
				} else if e.opts.PersistentCache {
					if a, ok, err := e.store.LookupCache(ctx, key, e.opts.AlgorithmVersion); err == nil && ok &&
						len(a.Pixels) == rect.W*rect.H*e.baseline.Channels {
						pixels = append([]byte(nil), a.Pixels...)
						hit = true
						result.Cache.PersistentHits++
					}
				}
			}
			if hit {
				result.Cache.Hits++
			} else {
				if e.opts.Fault != nil {
					if err := e.opts.Fault(t, rect); err != nil {
						return nil, core.Stage(t, checkpoint.BlockRef(b, rect), "injected stage failure: %v", err)
					}
				}
				required := fullFrameScratch(e.baseline)
				release, waits, err := e.buffers.Acquire(ctx, required, t, b, "pixel buffer exhausted")
				if err != nil {
					return nil, core.Stage(t, checkpoint.BlockRef(b, rect), "pixel buffer unavailable: %v", err)
				}
				_ = waits
				bf, err := ops.ApplyBlock(prevNew, step, rect, ops.Context{
					Seed: e.opts.ImageSeed, StepIndex: t - 1,
				})
				release()
				if err != nil {
					return nil, core.Stage(t, checkpoint.BlockRef(b, rect), "%v", err)
				}
				pixels = append([]byte(nil), bf.Pix...)
				e.cache.Put(key, BlockValue{
					Pixels: pixels, CacheKey: key, InputHash: inputHash,
					OutputHash: checkpoint.PixelHash(pixels),
				})
				result.Cache.Misses++
			}
			if err := setFrameBlock(frame, rect, pixels, e.baseline.Channels); err != nil {
				return nil, core.Stage(t, checkpoint.BlockRef(b, rect), "%v", err)
			}
			newKeys[t][b] = key
			newInputs[t][b] = inputHash
			oldPixels := oldFrames[t].BlockBytes(rect)
			if !bytes.Equal(oldPixels, pixels) {
				nextDirty[b] = true
				everChanged[b] = true
			}
			result.RecomputedBlocks++
			result.Invalidations = append(result.Invalidations, Invalidation{
				Step: t, Block: blockPtr(checkpoint.BlockRef(b, rect)),
				Reason: reason,
			})
		}
		for b := range carry {
			processed[b] = true
			oldPixels := oldFrames[t].BlockBytes(e.grid[b])
			newPixels := frame.BlockBytes(e.grid[b])
			if !bytes.Equal(oldPixels, newPixels) {
				nextDirty[b] = true
				everChanged[b] = true
			}
		}
		// Cache keys are metadata: update them even for identity blocks so a
		// downstream key always reflects this step's index and exact params.
		for b, rect := range e.grid {
			if processed[b] {
				continue
			}
			inputRegion := rect
			if radius := model.InputSupportRadius(step); radius > 0 {
				inputRegion = rect.ExpandClamped(radius, e.baseline.Width, e.baseline.Height)
			}
			inputHash := core.HashFrameRegion(prevNew, inputRegion)
			newInputs[t][b] = inputHash
			newKeys[t][b] = CacheKey(e.opts.AlgorithmVersion, t, inputHash, step.CanonicalParams(), newKeys[t-1][b])
		}
		if len(touched) > 0 {
			if err := e.finishEditStep(t, touched); err != nil {
				return nil, err
			}
			result.RecomputedSteps++
		}
		newFrames[t] = frame
		dirty = nextDirty
	}
	result.UnaffectedReused = len(e.grid)*n - result.RecomputedBlocks
	evals := append([]quality.BlockEvaluation(nil), e.evals...)
	for b := range everChanged {
		evals[b] = quality.EvaluateBlock(e.baseline, newFrames[1:], e.grid[b],
			e.opts.AttributionShare, newChain)
	}
	e.chain = newChain.Clone()
	e.frames = newFrames
	e.keys = newKeys
	e.inputHashes = newInputs
	e.evals = evals
	e.traces = buildTraces(e.baseline, e.chain, newFrames, len(e.grid))
	e.revision++
	result.VersionID = fmt.Sprintf("v%d", e.revision)
	result.Evaluations = evals
	result.FinalFrameHash = newFrames[n].Hash()
	result.Cache.Unaffected = result.UnaffectedReused
	e.versions = append(e.versions, Version{
		ID: result.VersionID, Chain: e.chain.Clone(), Frames: e.frames,
		Keys: cloneKeys(e.keys), InputHashes: cloneKeys(e.inputHashes),
		Evaluations: e.evals, Cache: result.Cache,
	})
	return result, nil
}

func (e *Engine) prepareEditStep(step int, touched map[int]bool) error {
	e.stateMu.Lock()
	defer e.stateMu.Unlock()
	sm := e.stepMachines[step-1]
	if sm.State() == core.StateCommitted {
		if err := sm.Transition(core.StatePending); err != nil {
			return err
		}
	}
	if sm.State() == core.StateFailed {
		if err := sm.Transition(core.StatePending); err != nil {
			return err
		}
	}
	if sm.State() == core.StatePending {
		if err := sm.Transition(core.StateProcessing); err != nil {
			return err
		}
	}
	for b := range touched {
		m := e.blockMachines[step-1][b]
		if m.State() == core.StateCommitted || m.State() == core.StateFailed {
			if err := m.Transition(core.StatePending); err != nil {
				return err
			}
		}
		if m.State() == core.StatePending {
			if err := m.Transition(core.StateProcessing); err != nil {
				return err
			}
		}
	}
	return nil
}

func (e *Engine) finishEditStep(step int, touched map[int]bool) error {
	e.stateMu.Lock()
	defer e.stateMu.Unlock()
	for b := range touched {
		m := e.blockMachines[step-1][b]
		if m.State() == core.StateProcessing {
			if err := m.Transition(core.StateCommitted); err != nil {
				return err
			}
		}
	}
	sm := e.stepMachines[step-1]
	if sm.State() == core.StateProcessing {
		return sm.Transition(core.StateCommitted)
	}
	return nil
}

func (e *Engine) noopEdit() (*EditResult, error) {
	n := len(e.chain.Steps)
	return &EditResult{
		VersionID:        fmt.Sprintf("v%d", e.revision),
		ChainFingerprint: model.ChainFingerprint(e.chain, e.opts.AlgorithmVersion),
		Evaluations:      e.evals,
		FinalFrameHash:   e.frames[n].Hash(),
	}, nil
}

func buildTraces(base *core.Frame, chain model.Chain, frames []*core.Frame, blocks int) []StepTrace {
	traces := make([]StepTrace, len(chain.Steps))
	for i, step := range chain.Steps {
		t := i + 1
		action := step.RegionRect(base.Width, base.Height)
		input := action.ExpandClamped(model.InputSupportRadius(step), base.Width, base.Height)
		traces[i] = StepTrace{
			Step: t, Type: string(step.Type), InputRegion: input, ActionRegion: action,
			OutputRegion: action, InputDigest: frames[t-1].Hash(), OutputDigest: frames[t].Hash(),
			SupportRadius: model.InputSupportRadius(step), BlockCount: blocks,
		}
	}
	return traces
}

func intersectionBlocks(grid []core.Rect, rects ...core.Rect) map[int]bool {
	out := make(map[int]bool)
	for _, r := range rects {
		markIntersections(out, grid, r)
	}
	return out
}

func markIntersections(out map[int]bool, grid []core.Rect, action core.Rect) {
	for i, block := range grid {
		if action.Intersects(block) {
			out[i] = true
		}
	}
}

func sortedInts(m map[int]bool) []int {
	out := make([]int, 0, len(m))
	for v := range m {
		out = append(out, v)
	}
	sort.Ints(out)
	return out
}

func blockFrame(f *core.Frame, rect core.Rect) *core.Frame {
	block, err := f.Block(rect)
	if err != nil {
		return &core.Frame{Width: rect.W, Height: rect.H, Channels: f.Channels, Pix: make([]byte, rect.W*rect.H*f.Channels)}
	}
	return block
}

func blockPtr(ref core.BlockRef) *core.BlockRef {
	return &ref
}

func minInt(a, b int) int {
	if a < b {
		return a
	}
	return b
}

func maxInt(a, b int) int {
	if a > b {
		return a
	}
	return b
}
