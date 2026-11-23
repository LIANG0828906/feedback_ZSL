package morph

import (
	"context"
	"errors"
	"fmt"
	"math"
	"runtime"
	"sync"
	"time"
)

type FaultPlan map[int]map[string]Stage

type RunOptions struct {
	Faults           FaultPlan
	StopAfterCommits int
}

type ResourceEvent struct {
	Kind      string `json:"kind"`
	Reason    string `json:"reason"`
	Requested int64  `json:"requested_bytes"`
	Capacity  int64  `json:"capacity_bytes"`
	Sample    int    `json:"sample"`
}

type RunSummary struct {
	Quality          Quality         `json:"quality"`
	TotalSamples     int             `json:"total_samples"`
	CommittedSamples int             `json:"committed_samples"`
	ResumedSamples   int             `json:"resumed_samples"`
	ComputedSamples  int             `json:"computed_samples"`
	ResourceEvents   []ResourceEvent `json:"resource_events,omitempty"`
	ElapsedMillis    int64           `json:"elapsed_ms"`
	Stopped          bool            `json:"stopped"`
}

type sampleWork struct {
	flats   []*FlattenedPath
	rasters []*PathRaster
	frame   FrameGeometry
	stats   []PathFrameStats
}

type Engine struct {
	Scene    *Scene
	Config   Config
	Quality  Quality
	Timeline *Timeline
	Store    *CheckpointStore

	mu      sync.RWMutex
	results []*SampleResult
	works   map[int]*sampleWork
	events  []ResourceEvent

	runMu    sync.Mutex
	ctrlMu   sync.Mutex
	paused   bool
	canceled bool
	cancelFn context.CancelFunc
	gov      *bufferGovernor
}

func DefaultConfigForScene(scene *Scene) Config {
	cfg := DefaultConfig()
	if scene != nil {
		if scene.Render.Scale > 0 {
			cfg.Eval.Scale = scene.Render.Scale
		}
		cfg.Render.PaddingPixels = scene.Render.PaddingPixels
		if scene.Render.MaxFramePixels > 0 {
			cfg.Render.MaxFramePixels = scene.Render.MaxFramePixels
		}
	}
	cfg.Adaptive.BaseGrid = cfg.Render.Grid
	cfg.Adaptive.BaseMidTolerance = cfg.Eval.MidTolerance
	if cfg.Runtime.Workers <= 0 {
		cfg.Runtime.Workers = runtime.NumCPU()
	}
	if cfg.Runtime.PerThreadPixelBytes <= 0 {
		cfg.Runtime.PerThreadPixelBytes = 64 << 20
	}
	return cfg
}

func NewEngine(scene *Scene, cfg Config) (*Engine, error) {
	if err := ValidateScene(scene); err != nil {
		return nil, err
	}
	if cfg.Eval.Interpolation == "" {
		cfg.Eval.Interpolation = "linear"
	}
	if cfg.Eval.Scale == 0 {
		cfg.Eval.Scale = scene.Render.Scale
	}
	if cfg.Render.Grid == 0 {
		cfg.Render.Grid = 4
	}
	if cfg.Render.MaxFramePixels == 0 {
		cfg.Render.MaxFramePixels = scene.Render.MaxFramePixels
	}
	if cfg.Runtime.Workers <= 0 {
		cfg.Runtime.Workers = 1
	}
	if cfg.Runtime.PerThreadPixelBytes <= 0 {
		cfg.Runtime.PerThreadPixelBytes = 64 << 20
	}
	if err := ValidateEvalConfig(cfg.Eval, scene); err != nil {
		return nil, err
	}
	if err := ValidateRenderConfig(cfg.Render); err != nil {
		return nil, err
	}
	q, err := DecideQuality(scene, cfg)
	if err != nil {
		return nil, err
	}
	store, err := OpenCheckpoint(cfg.Checkpoint, scene)
	if err != nil {
		return nil, err
	}
	committed, err := store.LoadCommitted()
	if err != nil {
		return nil, err
	}
	if len(committed) > len(scene.SampleTimes) {
		return nil, withField(newError(CodeDiverge, "checkpoint contains more samples than the scene"), "checkpoint")
	}
	tl := NewTimeline(scene)
	e := &Engine{
		Scene: scene, Config: cfg, Quality: q, Timeline: tl, Store: store,
		results: make([]*SampleResult, len(scene.SampleTimes)),
		works:   map[int]*sampleWork{},
	}
	totalCap := cfg.Runtime.PerThreadPixelBytes * int64(cfg.Runtime.Workers)
	if cfg.Adaptive.MemoryBudgetBytes > 0 && cfg.Adaptive.MemoryBudgetBytes < totalCap {
		totalCap = cfg.Adaptive.MemoryBudgetBytes
	}
	e.gov = newBufferGovernor(totalCap, cfg.Runtime.PerThreadPixelBytes)
	for i, res := range committed {
		if math.Abs(res.Time-scene.SampleTimes[i]) > 1e-12 {
			return nil, withField(newError(CodeDiverge, "checkpoint sample time mismatch"), "checkpoint.time")
		}
		if err := markLoadedCommitted(tl, i); err != nil {
			return nil, err
		}
		e.results[i] = res
	}
	return e, nil
}

func markLoadedCommitted(tl *Timeline, sample int) error {
	tl.mu.Lock()
	defer tl.mu.Unlock()
	for pi, id := range tl.pathIDs {
		for _, stage := range []Stage{StageFlatten, StageCoverage, StageComposite} {
			si := stageIndex(stage)
			if tl.states[sample][pi][si] != StatePending {
				return withPath(withField(newError(CodeDiverge, "loaded sample state is not pending"), "state"), id)
			}
			tl.states[sample][pi][si] = StateCommitted
		}
	}
	tl.committed[sample] = true
	tl.commitOrder = append(tl.commitOrder, sample)
	return nil
}

func (e *Engine) Run(ctx context.Context, opts RunOptions) (*RunSummary, error) {
	e.runMu.Lock()
	defer e.runMu.Unlock()
	started := time.Now()
	e.ctrlMu.Lock()
	e.paused = false
	e.canceled = false
	runCtx, cancel := context.WithCancel(ctx)
	e.cancelFn = cancel
	e.ctrlMu.Unlock()
	defer cancel()

	start := e.Store.CommittedCount()
	e.Timeline.AbortInFlight(start)
	total := len(e.Scene.SampleTimes)
	summary := &RunSummary{Quality: e.Quality, TotalSamples: total, ResumedSamples: start}
	if opts.StopAfterCommits > 0 && start >= opts.StopAfterCommits {
		summary.CommittedSamples = start
		summary.Stopped = true
		summary.ElapsedMillis = time.Since(started).Milliseconds()
		return summary, nil
	}
	if start >= total {
		summary.CommittedSamples = total
		summary.ElapsedMillis = time.Since(started).Milliseconds()
		return summary, nil
	}
	workers := e.Config.Runtime.Workers
	if workers > total-start {
		workers = total - start
	}
	if workers < 1 {
		workers = 1
	}
	jobs := make(chan int)
	out := make(chan sampleOut, workers*2)
	var wg sync.WaitGroup
	for w := 0; w < workers; w++ {
		wg.Add(1)
		go func(workerID int) {
			defer wg.Done()
			for idx := range jobs {
				if err := e.waitPause(runCtx); err != nil {
					return
				}
				res, work, err := e.processSample(runCtx, idx, opts.Faults, workerID)
				out <- sampleOut{index: idx, result: res, work: work, err: err}
				if err != nil {
					return
				}
			}
		}(w)
	}
	go func() {
		defer close(jobs)
		for i := start; i < total; i++ {
			select {
			case jobs <- i:
			case <-runCtx.Done():
				return
			}
		}
	}()
	go func() {
		wg.Wait()
		close(out)
	}()

	pending := map[int]sampleOut{}
	next := start
	var firstErr error
	stopped := false
	for item := range out {
		pending[item.index] = item
		for {
			current, ok := pending[next]
			if !ok {
				break
			}
			delete(pending, next)
			if stopped {
				next++
				continue
			}
			if firstErr != nil {
				next++
				continue
			}
			if current.err != nil {
				firstErr = current.err
				cancel()
				next++
				continue
			}
			if err := e.Store.CommitSample(current.result); err != nil {
				firstErr = err
				cancel()
				next++
				continue
			}
			if err := e.Timeline.CommitSample(current.index); err != nil {
				firstErr = err
				cancel()
				next++
				continue
			}
			e.mu.Lock()
			e.results[current.index] = current.result
			e.works[current.index] = current.work
			e.mu.Unlock()
			summary.ComputedSamples++
			summary.CommittedSamples = e.Store.CommittedCount()
			next++
			if opts.StopAfterCommits > 0 && summary.CommittedSamples >= opts.StopAfterCommits {
				stopped = true
				cancel()
			}
		}
	}
	summary.CommittedSamples = e.Store.CommittedCount()
	summary.Stopped = stopped
	summary.ElapsedMillis = time.Since(started).Milliseconds()
	e.mu.RLock()
	summary.ResourceEvents = append([]ResourceEvent(nil), e.events...)
	e.mu.RUnlock()
	if stopped {
		return summary, nil
	}
	if firstErr != nil {
		return summary, firstErr
	}
	if runCtx.Err() != nil && !stopped && summary.CommittedSamples < total {
		if errors.Is(runCtx.Err(), context.Canceled) {
			return summary, runCtx.Err()
		}
		return summary, runCtx.Err()
	}
	if summary.CommittedSamples < total && !stopped {
		return summary, withField(newError(CodeState, "run ended before all samples committed"), "sample")
	}
	return summary, nil
}

type sampleOut struct {
	index  int
	result *SampleResult
	work   *sampleWork
	err    error
}

func (e *Engine) processSample(ctx context.Context, index int, faults FaultPlan, workerID int) (*SampleResult, *sampleWork, error) {
	if err := ctx.Err(); err != nil {
		return nil, nil, err
	}
	t := e.Scene.SampleTimes[index]
	work := e.getWork(index)
	n := len(e.Scene.Paths)
	if cap(work.flats) < n {
		work.flats = make([]*FlattenedPath, n)
		work.rasters = make([]*PathRaster, n)
		work.stats = make([]PathFrameStats, n)
	}
	if len(work.flats) < n {
		work.flats = append(work.flats, make([]*FlattenedPath, n-len(work.flats))...)
		work.rasters = append(work.rasters, make([]*PathRaster, n-len(work.rasters))...)
		work.stats = append(work.stats, make([]PathFrameStats, n-len(work.stats))...)
	}
	var firstErr error
	geometry := EmptyBBox()
	haveAllFlats := true
	for pi := range e.Scene.Paths {
		if err := ctx.Err(); err != nil {
			return nil, work, err
		}
		p := &e.Scene.Paths[pi]
		state := e.Timeline.State(index, p.ID, StageFlatten)
		if state == StateCommitted {
			if work.flats[pi] == nil {
				err := withSample(withPath(withField(newError(CodeDiverge, "committed flatten stage has no frozen polyline"), "flatten"), p.ID), index, t)
				if firstErr == nil {
					firstErr = err
				}
				haveAllFlats = false
				continue
			}
			geometry = geometry.Union(work.flats[pi].StrokeBBox)
			continue
		}
		if err := e.beginStage(index, p.ID, StageFlatten, t); err != nil {
			if firstErr == nil {
				firstErr = err
			}
			haveAllFlats = false
			continue
		}
		if faulted(faults, index, p.ID, StageFlatten) {
			err := e.failStage(index, p.ID, StageFlatten, t, "injected flatten failure")
			if firstErr == nil {
				firstErr = err
			}
			haveAllFlats = false
			continue
		}
		flat, err := FlattenPath(p, t, EvalConfig{Interpolation: e.Config.Eval.Interpolation, Scale: e.Config.Eval.Scale, MidTolerance: e.Quality.MidTolerance})
		if err != nil {
			e.failStage(index, p.ID, StageFlatten, t, err.Error())
			if firstErr == nil {
				firstErr = annotateSample(err, index, t)
			}
			haveAllFlats = false
			continue
		}
		work.flats[pi] = flat
		work.stats[pi] = PathFrameStats{
			PathID: p.ID, SegmentCount: flat.SegmentCount,
			InterpolationCount: flat.InterpolationCount, GeometryBBox: flat.GeometryBBox,
		}
		if err := e.Timeline.Transition(index, p.ID, StageFlatten, StateCommitted); err != nil {
			if firstErr == nil {
				firstErr = err
			}
			haveAllFlats = false
			continue
		}
		geometry = geometry.Union(flat.StrokeBBox)
	}
	frame, err := DeriveFrameGeometryAligned(geometry, e.Config.Eval.Scale, e.Config.Render.PaddingPixels, e.Config.Render.MaxFramePixels, e.Quality.Grid)
	if err != nil {
		if firstErr == nil {
			firstErr = annotateSample(err, index, t)
		}
		return nil, work, firstErr
	}
	work.frame = frame
	estimated := int64(frame.Width*frame.Height*32) + int64(frame.Width*frame.Height*8)
	for pi := range work.flats {
		if work.flats[pi] != nil {
			estimated += int64(len(work.flats[pi].Points)) * 32
		}
	}
	waited, err := e.gov.acquire(ctx, workerID, estimated)
	if err != nil {
		if firstErr == nil {
			firstErr = annotateSample(err, index, t)
		}
		return nil, work, firstErr
	}
	if waited {
		_, capBytes := e.gov.stats()
		e.recordEvent(ResourceEvent{
			Kind: "pixel_buffer_wait", Reason: "per-thread/global pixel buffer exhausted; sample queued",
			Requested: estimated, Capacity: capBytes, Sample: index,
		})
	}
	defer e.gov.release(workerID, estimated)

	chunk := e.Quality.ChunkSize
	if chunk <= 0 {
		chunk = len(e.Scene.Paths)
	}
	for start := 0; start < len(e.Scene.Paths); start += chunk {
		if err := e.waitPause(ctx); err != nil {
			return nil, work, err
		}
		end := minInt(start+chunk, len(e.Scene.Paths))
		for pi := start; pi < end; pi++ {
			p := &e.Scene.Paths[pi]
			if work.flats[pi] == nil {
				continue
			}
			state := e.Timeline.State(index, p.ID, StageCoverage)
			if state == StateCommitted {
				if work.rasters[pi] == nil {
					err := withSample(withPath(withField(newError(CodeDiverge, "committed coverage stage has no frozen raster"), "coverage"), p.ID), index, t)
					if firstErr == nil {
						firstErr = err
					}
				}
				continue
			}
			if err := e.beginStage(index, p.ID, StageCoverage, t); err != nil {
				if firstErr == nil {
					firstErr = err
				}
				continue
			}
			if faulted(faults, index, p.ID, StageCoverage) {
				err := e.failStage(index, p.ID, StageCoverage, t, "injected coverage failure")
				if firstErr == nil {
					firstErr = err
				}
				continue
			}
			raster, err := RasterizePath(p, work.flats[pi], frame, e.Quality.Grid)
			if err != nil {
				e.failStage(index, p.ID, StageCoverage, t, err.Error())
				if firstErr == nil {
					firstErr = annotateSample(err, index, t)
				}
				continue
			}
			work.rasters[pi] = raster
			work.stats[pi] = raster.Stats
			if err := e.Timeline.Transition(index, p.ID, StageCoverage, StateCommitted); err != nil {
				if firstErr == nil {
					firstErr = err
				}
			}
		}
	}

	canvas := make([]float64, frame.Width*frame.Height)
	haveAllComposite := true
	for start := 0; start < len(e.Scene.Paths); start += chunk {
		if err := e.waitPause(ctx); err != nil {
			return nil, work, err
		}
		end := minInt(start+chunk, len(e.Scene.Paths))
		for pi := start; pi < end; pi++ {
			p := &e.Scene.Paths[pi]
			if work.rasters[pi] == nil {
				haveAllComposite = false
				continue
			}
			state := e.Timeline.State(index, p.ID, StageComposite)
			if state == StateCommitted {
				compositeRaster(canvas, frame.Width, frame.Height, p, work.rasters[pi])
				continue
			}
			if err := e.beginStage(index, p.ID, StageComposite, t); err != nil {
				if firstErr == nil {
					firstErr = err
				}
				haveAllComposite = false
				continue
			}
			if faulted(faults, index, p.ID, StageComposite) {
				err := e.failStage(index, p.ID, StageComposite, t, "injected composite failure")
				if firstErr == nil {
					firstErr = err
				}
				haveAllComposite = false
				continue
			}
			compositeRaster(canvas, frame.Width, frame.Height, p, work.rasters[pi])
			if err := e.Timeline.Transition(index, p.ID, StageComposite, StateCommitted); err != nil {
				if firstErr == nil {
					firstErr = err
				}
				haveAllComposite = false
			}
		}
	}
	if firstErr != nil || !haveAllFlats || !haveAllComposite {
		if firstErr == nil {
			firstErr = withSample(withField(newError(CodeState, "sample has incomplete stages"), "state"), index, t)
		}
		return nil, work, firstErr
	}
	gray := make([]uint8, len(canvas))
	for i, v := range canvas {
		if v < 0 {
			v = 0
		}
		if v > 1 {
			v = 1
		}
		gray[i] = uint8(math.Round(v * 255))
	}
	stats := make([]PathFrameStats, 0, len(work.stats))
	for _, s := range work.stats {
		stats = append(stats, s)
	}
	res := &SampleResult{
		Index: index, Time: t, Width: frame.Width, Height: frame.Height, Frame: frame,
		Quality: e.Quality, Gray: gray, PathStats: stats, PixelHash: HashBytes(gray),
		Mode: e.Quality.Mode, Algorithm: AlgorithmVersion,
		ConfigVersion: ConfigVersion(e.Config), StyleVersion: SceneStyleVersion(e.Scene),
	}
	res.Flattened = append(res.Flattened, work.flats...)
	res.ContentHash = sampleContentHash(res)
	return res, work, nil
}

func (e *Engine) beginStage(sample int, pathID string, stage Stage, t float64) error {
	state := e.Timeline.State(sample, pathID, stage)
	var err error
	switch state {
	case StatePending:
		err = e.Timeline.Transition(sample, pathID, stage, StateProcessing)
	case StateFailed:
		err = e.Timeline.Transition(sample, pathID, stage, StateProcessing)
	case StateProcessing:
		err = withSample(withPath(withField(newError(CodeState, "stage is already processing"), "state"), pathID), sample, t)
	default:
		err = withSample(withPath(withField(newError(CodeState, "stage cannot be started from committed state"), "state"), pathID), sample, t)
	}
	return err
}

func (e *Engine) failStage(sample int, pathID string, stage Stage, t float64, message string) error {
	_ = e.Timeline.Transition(sample, pathID, stage, StateFailed)
	e.Timeline.mu.Lock()
	e.Timeline.errors[e.Timeline.stateKey(sample, pathID, stage)] = message
	e.Timeline.mu.Unlock()
	return withSample(withPath(withField(newError(CodeState, "stage failed: "+message), string(stage)), pathID), sample, t)
}

func annotateSample(err error, sample int, t float64) error {
	if e, ok := AsError(err); ok {
		e.Sample = sample
		e.Time = t
		return e
	}
	return withSample(newError(CodeState, err.Error()), sample, t)
}

func faulted(plan FaultPlan, sample int, pathID string, stage Stage) bool {
	if plan == nil {
		return false
	}
	return plan[sample] != nil && plan[sample][pathID] == stage
}

func (e *Engine) getWork(index int) *sampleWork {
	e.mu.Lock()
	defer e.mu.Unlock()
	if w := e.works[index]; w != nil {
		return w
	}
	w := &sampleWork{}
	e.works[index] = w
	return w
}

func (e *Engine) Result(index int) *SampleResult {
	e.mu.RLock()
	defer e.mu.RUnlock()
	if index < 0 || index >= len(e.results) {
		return nil
	}
	return e.results[index]
}

func (e *Engine) Results() []*SampleResult {
	e.mu.RLock()
	defer e.mu.RUnlock()
	return append([]*SampleResult(nil), e.results...)
}

func (e *Engine) Pause() {
	e.ctrlMu.Lock()
	e.paused = true
	e.ctrlMu.Unlock()
}

func (e *Engine) Resume() {
	e.ctrlMu.Lock()
	e.paused = false
	e.ctrlMu.Unlock()
}

func (e *Engine) Paused() bool {
	e.ctrlMu.Lock()
	defer e.ctrlMu.Unlock()
	return e.paused
}

func (e *Engine) Cancel() {
	e.ctrlMu.Lock()
	e.canceled = true
	cancel := e.cancelFn
	e.paused = false
	e.ctrlMu.Unlock()
	if cancel != nil {
		cancel()
	}
}

func (e *Engine) waitPause(ctx context.Context) error {
	for {
		e.ctrlMu.Lock()
		paused := e.paused
		canceled := e.canceled
		e.ctrlMu.Unlock()
		if canceled {
			return context.Canceled
		}
		if !paused {
			return nil
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(10 * time.Millisecond):
		}
	}
}

// RetrySample retries only the next uncommitted sample. Committed stages are
// reused from the frozen work cache; failed stages are transitioned back to
// processing through the state machine.
func (e *Engine) RetrySample(ctx context.Context, index int, faults FaultPlan) (*SampleResult, error) {
	e.runMu.Lock()
	defer e.runMu.Unlock()
	if index != e.Store.CommittedCount() {
		e2 := withField(newError(CodeState, "only the next uncommitted sample can be retried"), "sample")
		e2.Sample = index
		return nil, e2
	}
	e.ctrlMu.Lock()
	runCtx, cancel := context.WithCancel(ctx)
	e.cancelFn = cancel
	e.paused = false
	e.canceled = false
	e.ctrlMu.Unlock()
	defer cancel()
	res, work, err := e.processSample(runCtx, index, faults, 0)
	if err != nil {
		return nil, err
	}
	if err := e.Store.CommitSample(res); err != nil {
		return nil, err
	}
	if err := e.Timeline.CommitSample(index); err != nil {
		return nil, err
	}
	e.mu.Lock()
	e.results[index] = res
	e.works[index] = work
	e.mu.Unlock()
	return res, nil
}

func (e *Engine) recordEvent(ev ResourceEvent) {
	e.mu.Lock()
	e.events = append(e.events, ev)
	e.mu.Unlock()
}

type bufferGovernor struct {
	mu         sync.Mutex
	capacity   int64
	threadCap  int64
	used       int64
	workerUsed map[int]int64
}

func newBufferGovernor(capacity, threadCap int64) *bufferGovernor {
	if capacity <= 0 {
		capacity = 1
	}
	if threadCap <= 0 {
		threadCap = capacity
	}
	return &bufferGovernor{capacity: capacity, threadCap: threadCap, workerUsed: map[int]int64{}}
}

func (g *bufferGovernor) acquire(ctx context.Context, worker int, n int64) (bool, error) {
	if n <= 0 {
		n = 1
	}
	if n > g.threadCap || n > g.capacity {
		e := withField(newError(CodeParam, "frame pixel buffer exceeds the configured worker limit"), "per_thread_pixel_bytes")
		e.Measured = int64String(n)
		e.Expected = int64String(minInt64(g.threadCap, g.capacity))
		return false, e
	}
	waited := false
	started := time.Now()
	for {
		g.mu.Lock()
		if g.used+n <= g.capacity && g.workerUsed[worker]+n <= g.threadCap {
			g.used += n
			g.workerUsed[worker] += n
			g.mu.Unlock()
			return waited, nil
		}
		g.mu.Unlock()
		waited = true
		if time.Since(started) > 30*time.Second {
			e := withField(newError(CodeParam, "timed out waiting for a pixel buffer reservation"), "per_thread_pixel_bytes")
			e.Measured = int64String(n)
			e.Expected = int64String(g.threadCap)
			return true, e
		}
		select {
		case <-ctx.Done():
			return waited, ctx.Err()
		case <-time.After(2 * time.Millisecond):
		}
	}
}

func minInt64(a, b int64) int64 {
	if a < b {
		return a
	}
	return b
}

func int64String(v int64) string {
	return fmt.Sprintf("%d", v)
}

func (g *bufferGovernor) release(worker int, n int64) {
	g.mu.Lock()
	defer g.mu.Unlock()
	g.used -= n
	g.workerUsed[worker] -= n
	if g.used < 0 {
		g.used = 0
	}
	if g.workerUsed[worker] <= 0 {
		delete(g.workerUsed, worker)
	}
}

func (g *bufferGovernor) stats() (used, capacity int64) {
	g.mu.Lock()
	defer g.mu.Unlock()
	return g.used, g.capacity
}
