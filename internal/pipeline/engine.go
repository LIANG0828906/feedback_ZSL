package pipeline

import (
	"fmt"
	"sort"
	"sync"
	"time"

	"volumekernel/internal/checkpoint"
	"volumekernel/internal/kernel"
	"volumekernel/internal/model"
)

type BlockCoord struct {
	X int
	Y int
}

type RenderOptions struct {
	CameraIndex        int
	Mode               string
	Steps              int
	ShadowSamples      int
	StepLimit          int
	ShadowSampleLimit  int
	MaxParallel        int
	UsePersistentCache bool
	UseCheckpoint      bool
	Resume             bool
	CommitCheckpoint   bool
	CommitLimit        int
	FailBlock          *BlockCoord
}

type RenderResult struct {
	Frame              model.Frame
	Session            checkpoint.SessionState
	CacheHits          int
	CacheMisses        int
	CheckpointReused   int
	StoppedEarly       bool
	PreviewDeadlineMet bool
}

type Engine struct {
	Store *checkpoint.Store
}

type workerOutcome struct {
	index      int
	block      model.BlockResult
	err        error
	cacheHit   bool
	checkpoint bool
	queue      []model.QueueEvent
}

type shadowGate struct {
	mu        sync.Mutex
	cond      *sync.Cond
	capacity  int
	available int
}

func newShadowGate(capacity int) *shadowGate {
	g := &shadowGate{capacity: capacity, available: capacity}
	g.cond = sync.NewCond(&g.mu)
	return g
}

func (g *shadowGate) acquire(n, blockIndex int, bx, by int) (model.QueueEvent, error) {
	if g == nil || g.capacity <= 0 || n <= 0 {
		return model.QueueEvent{}, nil
	}
	if n > g.capacity {
		e := model.NewError(model.EBudget, "block shadow requirement exceeds global budget")
		e.BlockX, e.BlockY = bx, by
		e.Actual, e.Limit = n, g.capacity
		return model.QueueEvent{}, e
	}
	g.mu.Lock()
	defer g.mu.Unlock()
	start := time.Now()
	waited := false
	for g.available < n {
		waited = true
		g.cond.Wait()
	}
	waitMS := float64(time.Since(start).Microseconds()) / 1000
	g.available -= n
	if !waited {
		return model.QueueEvent{}, nil
	}
	return model.QueueEvent{
		BlockIndex: blockIndex,
		Reason:     fmt.Sprintf("shadow budget queue: need=%d available_at_wait=%d", n, g.available+n),
		WaitMS:     waitMS,
		At:         start.UTC().Format(time.RFC3339Nano),
	}, nil
}

func (g *shadowGate) release(n int) {
	if g == nil || g.capacity <= 0 || n <= 0 {
		return
	}
	g.mu.Lock()
	g.available += n
	if g.available > g.capacity {
		g.available = g.capacity
	}
	g.cond.Broadcast()
	g.mu.Unlock()
}

func (e *Engine) Render(snapshot model.SceneSnapshot, opts RenderOptions) (RenderResult, error) {
	start := time.Now()
	mode := opts.Mode
	if mode == "" {
		mode = "exact"
	}
	steps := opts.Steps
	shadow := opts.ShadowSamples
	if steps <= 0 {
		if mode == "preview" {
			steps = snapshot.Scene.Recipe.PreviewSteps
		} else {
			steps = snapshot.Scene.Recipe.Steps
		}
	}
	if shadow <= 0 {
		if mode == "preview" {
			shadow = snapshot.Scene.Recipe.PreviewShadowSamples
		} else {
			shadow = snapshot.Scene.Recipe.ShadowSamples
		}
	}
	stepLimit := opts.StepLimit
	if stepLimit <= 0 {
		stepLimit = steps
	}
	maxParallel := opts.MaxParallel
	if maxParallel <= 0 {
		maxParallel = snapshot.Scene.Recipe.MaxParallelBlocks
	}
	plans := PlanBlocks(snapshot.Scene.Viewport, snapshot.Scene.Recipe)
	fps := make([]BlockFingerprint, len(plans))
	for i, p := range plans {
		fp, err := ComputeFingerprint(snapshot, opts.CameraIndex, p, mode, steps, shadow)
		if err != nil {
			return RenderResult{}, err
		}
		fps[i] = fp
	}

	meta := checkpoint.Meta{
		SceneHash:        snapshot.SceneHash,
		CameraVersion:    snapshot.CameraVersion,
		RecipeVersion:    snapshot.RecipeVersion,
		AlgorithmVersion: model.AlgorithmVersion,
	}
	result := RenderResult{}
	if opts.UseCheckpoint {
		if e.Store == nil {
			return result, model.ParamError("", "checkpoint", "checkpoint requested without a store")
		}
		st, err := e.Store.BeginSession(meta)
		if err != nil {
			return result, err
		}
		result.Session = st
		if !opts.Resume {
			if err := e.Store.ResetBlocks(meta.SessionID); err != nil {
				return result, err
			}
		}
	}
	preloaded := make(map[int]model.BlockResult)
	if opts.Resume {
		if e.Store == nil {
			return result, model.ParamError("", "resume", "resume requested without a checkpoint store")
		}
		recs, st, err := e.Store.LoadCommitted(meta)
		if err != nil {
			return result, err
		}
		if st.Invalidated {
			result.Session = st
		}
		for _, rec := range recs {
			if rec.Seq < 0 || rec.Seq >= len(plans) {
				continue
			}
			if rec.X != plans[rec.Seq].X || rec.Y != plans[rec.Seq].Y {
				return result, model.NewError(model.EDecode, "checkpoint block sequence does not match tile plan")
			}
			preloaded[rec.Seq] = rec.Block
		}
	}

	jobs := make(chan int)
	outcomes := make(chan workerOutcome, len(plans))
	gate := newShadowGate(snapshot.Scene.Recipe.ShadowBudget)
	var wg sync.WaitGroup
	workers := maxParallel
	if workers > len(plans) {
		workers = len(plans)
	}
	if workers < 1 && len(plans) > 0 {
		workers = 1
	}
	for w := 0; w < workers; w++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for idx := range jobs {
				plan := plans[idx]
				out := workerOutcome{index: idx}
				if opts.FailBlock != nil && plan.X == opts.FailBlock.X && plan.Y == opts.FailBlock.Y {
					out.err = model.BlockError(plan.X, plan.Y, fmt.Errorf("injected single-block failure"))
					outcomes <- out
					continue
				}
				if b, ok := preloaded[idx]; ok {
					out.block = b
					out.checkpoint = true
					outcomes <- out
					continue
				}
				if opts.UsePersistentCache && e.Store != nil {
					if b, _, ok, err := e.Store.GetCache(fps[idx].CacheKey, fps[idx].ParameterVersion, model.AlgorithmVersion); err != nil {
						out.err = err
					} else if ok {
						out.block = b
						out.cacheHit = true
						outcomes <- out
						continue
					}
				}
				block, q, err := e.renderBlock(snapshot, opts.CameraIndex, plan, fps[idx], steps, shadow, stepLimit, opts.ShadowSampleLimit, gate)
				out.block = block
				out.err = err
				out.queue = q
				if err == nil && opts.UsePersistentCache && e.Store != nil {
					_ = e.Store.PutCache(fps[idx].CacheKey, fps[idx].ParameterVersion, fps[idx].ContentHash, model.AlgorithmVersion, block, fps[idx].Deps)
				}
				outcomes <- out
			}
		}()
	}
	go func() {
		for i := range plans {
			jobs <- i
		}
		close(jobs)
		wg.Wait()
		close(outcomes)
	}()
	byIndex := make(map[int]workerOutcome, len(plans))
	for out := range outcomes {
		byIndex[out.index] = out
	}

	seq := 0
	var firstErr error
	var selected []workerOutcome
	for _, plan := range plans {
		out, ok := byIndex[plan.Index]
		if !ok {
			firstErr = model.BlockError(plan.X, plan.Y, fmt.Errorf("scheduler returned no result"))
			break
		}
		if out.err != nil {
			firstErr = out.err
			break
		}
		selected = append(selected, out)
		if out.cacheHit {
			result.CacheHits++
		} else {
			result.CacheMisses++
		}
		if out.checkpoint {
			result.CheckpointReused++
		}
		result.Frame.Queue = append(result.Frame.Queue, out.queue...)
		if opts.CommitCheckpoint && opts.UseCheckpoint && !out.checkpoint {
			rec := checkpoint.BlockRecord{
				Seq:         seq,
				X:           plan.X,
				Y:           plan.Y,
				RayDigest:   fps[plan.Index].RayDigest,
				CacheKey:    fps[plan.Index].CacheKey,
				ContentHash: snapshot.SceneHash,
				Recipe:      snapshot.RecipeVersion,
				Algorithm:   model.AlgorithmVersion,
				Block:       out.block,
			}
			if err := e.Store.CommitBlock(rec); err != nil {
				firstErr = err
				break
			}
		}
		seq++
		if opts.CommitLimit > 0 && seq >= opts.CommitLimit {
			result.StoppedEarly = seq < len(plans)
			break
		}
	}
	for _, out := range selected {
		result.Frame.Blocks = append(result.Frame.Blocks, out.block)
		result.Frame.Pixels = append(result.Frame.Pixels, out.block.Pixels...)
		result.Frame.ActualSteps += out.block.Steps
		result.Frame.ActualShadows += out.block.ShadowSamples
	}
	sort.Slice(result.Frame.Blocks, func(i, j int) bool { return result.Frame.Blocks[i].Index < result.Frame.Blocks[j].Index })
	sort.Slice(result.Frame.Pixels, func(i, j int) bool {
		a, b := result.Frame.Pixels[i], result.Frame.Pixels[j]
		if a.Y != b.Y {
			return a.Y < b.Y
		}
		return a.X < b.X
	})
	sort.Slice(result.Frame.Queue, func(i, j int) bool {
		if result.Frame.Queue[i].BlockIndex != result.Frame.Queue[j].BlockIndex {
			return result.Frame.Queue[i].BlockIndex < result.Frame.Queue[j].BlockIndex
		}
		return result.Frame.Queue[i].At < result.Frame.Queue[j].At
	})
	result.Frame.Mode = mode
	result.Frame.SnapshotVersion = snapshot.Version
	result.Frame.CameraVersion = snapshot.CameraVersion
	result.Frame.RecipeVersion = snapshot.RecipeVersion
	result.Frame.AlgorithmVersion = model.AlgorithmVersion
	result.Frame.Viewport = snapshot.Scene.Viewport
	result.Frame.StepBudget = stepLimit
	result.Frame.ShadowBudget = snapshot.Scene.Recipe.ShadowBudget
	result.Frame.ElapsedMS = float64(time.Since(start).Microseconds()) / 1000
	if mode == "preview" {
		result.PreviewDeadlineMet = result.Frame.ElapsedMS <= float64(snapshot.Scene.Recipe.PreviewLatencyMS)
	}
	if firstErr != nil {
		return result, firstErr
	}
	return result, nil
}

func (e *Engine) renderBlock(
	snapshot model.SceneSnapshot,
	cameraIndex int,
	plan BlockPlan,
	fp BlockFingerprint,
	steps, shadow, stepLimit, shadowLimit int,
	gate *shadowGate,
) (model.BlockResult, []model.QueueEvent, error) {
	needShadow := len(plan.Pixels) * steps * shadow
	q, err := gate.acquire(needShadow, plan.Index, plan.X, plan.Y)
	if err != nil {
		return model.BlockResult{}, nil, err
	}
	if needShadow > 0 {
		defer gate.release(needShadow)
	}
	var queue []model.QueueEvent
	if q.Reason != "" {
		queue = append(queue, q)
	}
	sample := snapshot.Scene.CameraTrack.Samples[cameraIndex]
	block := model.BlockResult{
		X:           plan.X,
		Y:           plan.Y,
		Index:       plan.Index,
		CacheKey:    fp.CacheKey,
		ContentHash: fp.ContentHash,
		RayDigest:   fp.RayDigest,
	}
	maxRays := snapshot.Scene.Recipe.MaxRaysPerBlock
	if maxRays <= 0 {
		maxRays = len(plan.Pixels)
	}
	for start := 0; start < len(plan.Pixels); start += maxRays {
		end := start + maxRays
		if end > len(plan.Pixels) {
			end = len(plan.Pixels)
		}
		buffer := make([]PixelCoord, end-start)
		copy(buffer, plan.Pixels[start:end])
		for _, p := range buffer {
			pr, _, err := kernel.RenderRay(snapshot.Scene, sample, snapshot.Scene.Viewport, p.X, p.Y, kernel.Options{
				Steps:             steps,
				ShadowSamples:     shadow,
				StepBudget:        stepLimit,
				ShadowSampleLimit: shadowLimit,
			})
			if err != nil {
				if ke, ok := err.(*model.Error); ok {
					ke.BlockX, ke.BlockY = plan.X, plan.Y
					ke.HasBlock = true
				}
				return model.BlockResult{}, queue, err
			}
			block.Pixels = append(block.Pixels, pr)
			block.Steps += pr.Steps
			block.ShadowSamples += pr.ShadowSamples
		}
	}
	for _, p := range block.Pixels {
		block.TransmittanceAvg += p.Transmittance
		block.ScatterAvg += p.ScatteredRadiance
	}
	if len(block.Pixels) > 0 {
		block.TransmittanceAvg /= float64(len(block.Pixels))
		block.ScatterAvg /= float64(len(block.Pixels))
	}
	return block, queue, nil
}
