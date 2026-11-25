package vectorcore

import (
	"context"
	"fmt"
	"sort"
	"sync"
)

type PathFrame struct {
	PathID         string     `json:"pathId"`
	Contour        Contour    `json:"contour"`
	Flattened      Polyline   `json:"flattened"`
	StrokeBand     []Polyline `json:"strokeBand,omitempty"`
	Bounds         Bounds     `json:"bounds"`
	AnalyticArea   int64      `json:"analyticArea"`
	Coverage       Coverage   `json:"coverage"`
	CompositeAfter *Bitmap    `json:"compositeAfter"`
}

type TimeResult struct {
	SceneID     string      `json:"sceneId"`
	TimeIndex   int         `json:"timeIndex"`
	TimeTicks   int64       `json:"timeTicks"`
	CommitSeq   int64       `json:"commitSeq"`
	ContentHash string      `json:"contentHash"`
	Viewport    Viewport    `json:"viewport"`
	Bitmap      *Bitmap     `json:"bitmap"`
	Paths       []PathFrame `json:"paths"`
}

func (r *TimeResult) Clone() *TimeResult {
	if r == nil {
		return nil
	}
	cp := *r
	cp.Bitmap = r.Bitmap.Clone()
	cp.Paths = make([]PathFrame, len(r.Paths))
	for i, p := range r.Paths {
		cp.Paths[i] = p
		cp.Paths[i].Flattened = p.Flattened.Clone()
		cp.Paths[i].StrokeBand = make([]Polyline, len(p.StrokeBand))
		for j, ring := range p.StrokeBand {
			cp.Paths[i].StrokeBand[j] = ring.Clone()
		}
		cp.Paths[i].Coverage = p.Coverage.Clone()
		cp.Paths[i].CompositeAfter = p.CompositeAfter.Clone()
	}
	return &cp
}

type TimelineResult struct {
	SceneID string        `json:"sceneId"`
	Times   []*TimeResult `json:"times"`
}

func (t *TimelineResult) Clone() *TimelineResult {
	if t == nil {
		return nil
	}
	cp := &TimelineResult{SceneID: t.SceneID, Times: make([]*TimeResult, len(t.Times))}
	for i, r := range t.Times {
		cp.Times[i] = r.Clone()
	}
	return cp
}

type FaultHook func(timeIndex int, pathID string, stage Stage) error

type Engine struct {
	mu        sync.Mutex
	scene     *Scene
	cfg       Config
	cache     *stageCache
	states    *stateMachine
	resultsMu sync.Mutex
	results   map[int]*TimeResult
	nextSeq   int64
	commitMu  sync.Mutex
	store     *CheckpointStore
	faultMu   sync.Mutex
	fault     FaultHook
	rendering bool
}

func NewEngine(scene *Scene, cfg Config) (*Engine, error) {
	if err := cfg.Validate(); err != nil {
		return nil, err
	}
	if err := scene.Validate(); err != nil {
		return nil, err
	}
	cp := scene.Clone()
	e := &Engine{
		scene:   cp,
		cfg:     cfg,
		cache:   newStageCache(),
		states:  newStateMachine(),
		results: make(map[int]*TimeResult),
	}
	for ti := range cp.SampleTimes {
		for pi := range cp.Paths {
			e.states.init(ti, cp.Paths[pi].ID)
		}
	}
	return e, nil
}

func (e *Engine) Config() Config { return e.cfg }

func (e *Engine) Scene() *Scene {
	e.mu.Lock()
	defer e.mu.Unlock()
	return e.scene.Clone()
}

func (e *Engine) SetFaultHook(h FaultHook) {
	e.faultMu.Lock()
	e.fault = h
	e.faultMu.Unlock()
}

func (e *Engine) injectFault(timeIndex int, pathID string, stage Stage) error {
	e.faultMu.Lock()
	h := e.fault
	e.faultMu.Unlock()
	if h == nil {
		return nil
	}
	return h(timeIndex, pathID, stage)
}

func (e *Engine) beginRender() func() {
	e.mu.Lock()
	e.rendering = true
	return func() {
		e.rendering = false
		e.mu.Unlock()
	}
}

func (e *Engine) beginStage(useState bool, timeIndex int, pathID string, stage Stage) (bool, error) {
	if !useState {
		return false, nil
	}
	state := e.states.get(timeIndex, pathID, stage)
	switch state {
	case StateCommitted:
		return true, nil
	case StateFailed:
		if err := e.states.transition(timeIndex, pathID, stage, StatePending); err != nil {
			return false, err
		}
	case StateProcessing:
		if err := e.states.transition(timeIndex, pathID, stage, StateFailed); err != nil {
			return false, err
		}
		if err := e.states.transition(timeIndex, pathID, stage, StatePending); err != nil {
			return false, err
		}
	}
	if err := e.states.transition(timeIndex, pathID, stage, StateProcessing); err != nil {
		return false, err
	}
	return false, nil
}

func (e *Engine) finishStage(useState, committed bool, timeIndex int, pathID string, stage Stage) error {
	if !useState || committed {
		return nil
	}
	return e.states.transition(timeIndex, pathID, stage, StateCommitted)
}

func (e *Engine) failStage(useState bool, timeIndex int, pathID string, stage Stage) {
	if !useState {
		return
	}
	_ = e.states.transition(timeIndex, pathID, stage, StateFailed)
}

func (e *Engine) flattenParams() string {
	return HashValue(struct {
		Tolerance    int64
		Interp       InterpMode
		InterpVer    string
		ToleranceVer string
		Algorithm    string
	}{e.cfg.Tolerance, e.cfg.Interpolation, InterpolationVersion, ToleranceVersion, AlgorithmVersion})
}

func (e *Engine) flattenStage(timeIndex int, path EvaluatedPath, useState bool) (*pathGeometry, string, error) {
	inputHash := HashValue(struct {
		PathID    string
		Contour   Contour
		Fill      bool
		Width     int64
		Join      JoinMode
		Keyframes []int
	}{path.PathID, path.Contour, path.Paint.Fill, path.Paint.StrokeWidth, path.Paint.Join, path.KeyframeIndex})
	paramHash := e.flattenParams()
	key := stageCacheKey(inputHash, path.PathID, timeIndex, StageFlatten, paramHash, "")
	committed, err := e.beginStage(useState, timeIndex, path.PathID, StageFlatten)
	if err != nil {
		return nil, "", err
	}
	if committed {
		entry, ok := e.cache.get(key)
		if !ok || entry.geometry == nil {
			return nil, "", &Error{Code: CodeState, Op: "cache", PathID: path.PathID, TimeIndex: timeIndex, Stage: StageFlatten, Keyframe: -1,
				Err: fmt.Errorf("committed flatten entry is missing")}
		}
		return entry.geometry, key, nil
	}
	if err := e.injectFault(timeIndex, path.PathID, StageFlatten); err != nil {
		e.failStage(useState, timeIndex, path.PathID, StageFlatten)
		return nil, "", err
	}
	if entry, ok := e.cache.get(key); ok && entry.geometry != nil {
		if err := e.finishStage(useState, false, timeIndex, path.PathID, StageFlatten); err != nil {
			return nil, "", err
		}
		return entry.geometry, key, nil
	}
	geo, err := buildPathGeometry(path, e.cfg)
	if err != nil {
		e.failStage(useState, timeIndex, path.PathID, StageFlatten)
		return nil, "", err
	}
	entry := newCacheEntry(key, StageFlatten, path.PathID, timeIndex, -1, inputHash, paramHash, "")
	entry.geometry = geo
	entry.addDependency(StageFlatten, "", path.KeyframeIndex)
	e.cache.put(entry)
	if err := e.finishStage(useState, false, timeIndex, path.PathID, StageFlatten); err != nil {
		return nil, "", err
	}
	return geo, key, nil
}

func (e *Engine) coverageStage(timeIndex, layer int, pathID string, geo *pathGeometry, flattenKey string, vp Viewport, useState bool) (Coverage, string, error) {
	vpHash := HashValue(vp)
	inputHash := geo.ContentHash + ":" + vpHash
	paramHash := HashValue(struct {
		Grid         int
		FillRule     FillRule
		Algorithm    string
		ToleranceVer string
	}{e.cfg.Grid, e.cfg.FillRule, AlgorithmVersion, ToleranceVersion})
	key := stageCacheKey(inputHash, pathID, timeIndex, StageCoverage, paramHash, flattenKey)
	committed, err := e.beginStage(useState, timeIndex, pathID, StageCoverage)
	if err != nil {
		return Coverage{}, "", err
	}
	if committed {
		entry, ok := e.cache.get(key)
		if !ok || entry.coverage == nil {
			return Coverage{}, "", &Error{Code: CodeState, Op: "cache", PathID: pathID, TimeIndex: timeIndex, Stage: StageCoverage, Keyframe: -1,
				Err: fmt.Errorf("committed coverage entry is missing")}
		}
		return entry.coverage.Clone(), key, nil
	}
	if err := e.injectFault(timeIndex, pathID, StageCoverage); err != nil {
		e.failStage(useState, timeIndex, pathID, StageCoverage)
		return Coverage{}, "", err
	}
	if entry, ok := e.cache.get(key); ok && entry.coverage != nil {
		if err := e.finishStage(useState, false, timeIndex, pathID, StageCoverage); err != nil {
			return Coverage{}, "", err
		}
		return entry.coverage.Clone(), key, nil
	}
	cov, err := makePathCoverage(geo, vp, e.cfg)
	if err != nil {
		e.failStage(useState, timeIndex, pathID, StageCoverage)
		return Coverage{}, "", err
	}
	entry := newCacheEntry(key, StageCoverage, pathID, timeIndex, layer, inputHash, paramHash, flattenKey)
	entry.coverage = &cov
	entry.addDependency(StageCoverage, flattenKey, nil)
	e.cache.put(entry)
	if err := e.finishStage(useState, false, timeIndex, pathID, StageCoverage); err != nil {
		return Coverage{}, "", err
	}
	return cov.Clone(), key, nil
}

func (e *Engine) compositeStage(timeIndex, layer int, paint Paint, pathID string, cov Coverage, coverageKey, prevKey string, prev *Bitmap, useState bool) (*Bitmap, string, error) {
	paramHash := HashValue(struct {
		Color        RGBA
		Opacity      int
		Blend        BlendMode
		CompositeVer string
	}{paint.Color, paint.Opacity, paint.Blend, CompositeVersion})
	upstream := coverageKey + "|" + prevKey
	inputHash := HashValue(cov)
	key := stageCacheKey(inputHash, pathID, timeIndex, StageComposite, paramHash, upstream)
	committed, err := e.beginStage(useState, timeIndex, pathID, StageComposite)
	if err != nil {
		return nil, "", err
	}
	if committed {
		entry, ok := e.cache.get(key)
		if !ok || entry.composite == nil {
			return nil, "", &Error{Code: CodeState, Op: "cache", PathID: pathID, TimeIndex: timeIndex, Stage: StageComposite, Keyframe: -1,
				Err: fmt.Errorf("committed composite entry is missing")}
		}
		return entry.composite.Clone(), key, nil
	}
	if err := e.injectFault(timeIndex, pathID, StageComposite); err != nil {
		e.failStage(useState, timeIndex, pathID, StageComposite)
		return nil, "", err
	}
	if entry, ok := e.cache.get(key); ok && entry.composite != nil {
		if err := e.finishStage(useState, false, timeIndex, pathID, StageComposite); err != nil {
			return nil, "", err
		}
		return entry.composite.Clone(), key, nil
	}
	out, err := CompositePath(prev, cov, paint, pathID)
	if err != nil {
		e.failStage(useState, timeIndex, pathID, StageComposite)
		return nil, "", err
	}
	entry := newCacheEntry(key, StageComposite, pathID, timeIndex, layer, inputHash, paramHash, upstream)
	entry.composite = out.Clone()
	entry.addDependency(StageComposite, upstream, nil)
	e.cache.put(entry)
	if err := e.finishStage(useState, false, timeIndex, pathID, StageComposite); err != nil {
		return nil, "", err
	}
	return out, key, nil
}

func (e *Engine) renderTime(ctx context.Context, timeIndex int, useState bool) (*TimeResult, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	eval, err := EvaluateFrame(e.scene, timeIndex, e.cfg)
	if err != nil {
		return nil, err
	}
	geometries := make([]*pathGeometry, len(eval.Paths))
	flattenKeys := make([]string, len(eval.Paths))
	frameBounds := EmptyBounds()
	for pi := range eval.Paths {
		geo, key, err := e.flattenStage(timeIndex, eval.Paths[pi], useState)
		if err != nil {
			return nil, err
		}
		geometries[pi] = geo
		flattenKeys[pi] = key
		frameBounds.Union(geo.Bounds)
	}
	vp, err := DeriveViewport(frameBounds, e.cfg)
	if err != nil {
		return nil, err
	}
	if err := checkPixelBuffer(vp.Width, vp.Height, 4, e.cfg.MaxPixelBufferBytes); err != nil {
		return nil, err
	}
	blank, err := NewBitmap(vp.Width, vp.Height)
	if err != nil {
		return nil, err
	}
	result := &TimeResult{SceneID: e.scene.ID, TimeIndex: timeIndex, TimeTicks: eval.TimeTicks, Viewport: vp}
	prev := blank
	prevKey := "root"
	for pi := range eval.Paths {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		ep := eval.Paths[pi]
		geo := geometries[pi]
		cov, coverageKey, err := e.coverageStage(timeIndex, pi, ep.PathID, geo, flattenKeys[pi], vp, useState)
		if err != nil {
			return nil, err
		}
		composite, compositeKey, err := e.compositeStage(timeIndex, pi, ep.Paint, ep.PathID, cov, coverageKey, prevKey, prev, useState)
		if err != nil {
			return nil, err
		}
		result.Paths = append(result.Paths, PathFrame{
			PathID: ep.PathID, Contour: ep.Contour.Clone(), Flattened: geo.Flattened.Clone(),
			StrokeBand: cloneRings(geo.StrokeBand), Bounds: geo.Bounds, AnalyticArea: geo.AreaExact,
			Coverage: cov.Clone(), CompositeAfter: composite.Clone(),
		})
		prev = composite
		prevKey = compositeKey
	}
	result.Bitmap = prev.Clone()
	result.ContentHash = HashValue(struct {
		TimeTicks int64
		Viewport  Viewport
		Paths     []PathFrame
	}{result.TimeTicks, result.Viewport, result.Paths})
	if useState {
		ids := make([]string, len(eval.Paths))
		for i := range eval.Paths {
			ids[i] = eval.Paths[i].PathID
		}
		e.states.freezeTime(timeIndex, ids)
	}
	return result, nil
}

func cloneRings(rings []Polyline) []Polyline {
	out := make([]Polyline, len(rings))
	for i, r := range rings {
		out[i] = r.Clone()
	}
	return out
}

func (e *Engine) resultFor(timeIndex int) *TimeResult {
	e.resultsMu.Lock()
	defer e.resultsMu.Unlock()
	return e.results[timeIndex]
}

func (e *Engine) commitResult(ctx context.Context, result *TimeResult) error {
	e.commitMu.Lock()
	defer e.commitMu.Unlock()
	if existing := e.resultFor(result.TimeIndex); existing != nil {
		return nil
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	e.nextSeq++
	result.CommitSeq = e.nextSeq
	if e.store != nil {
		if err := e.store.appendWithConfig(ctx, result, e.sceneContentHash(), e.configHash()); err != nil {
			e.nextSeq--
			return err
		}
	}
	e.resultsMu.Lock()
	e.results[result.TimeIndex] = result
	e.resultsMu.Unlock()
	return nil
}

func (e *Engine) sceneContentHash() string {
	return HashValue(struct {
		ID          string
		FPS         int64
		Rounding    Rounding
		SampleTimes []int64
		Paths       []Path
	}{e.scene.ID, e.scene.FPS, e.scene.Rounding, e.scene.SampleTimes, e.scene.Paths})
}

// RenderAll renders every uncommitted sample time. Times are computed in
// parallel up to Config.MaxWorkers but committed in ascending sample order.
func (e *Engine) RenderAll(ctx context.Context) (*TimelineResult, error) {
	done := e.beginRender()
	defer done()
	pending := make([]int, 0, len(e.scene.SampleTimes))
	for i := range e.scene.SampleTimes {
		if e.resultFor(i) == nil {
			pending = append(pending, i)
		}
	}
	rendered := make([]*TimeResult, len(e.scene.SampleTimes))
	errs := make([]error, len(e.scene.SampleTimes))
	jobs := make(chan int)
	var wg sync.WaitGroup
	workers := e.cfg.MaxWorkers
	if workers > len(pending) {
		workers = len(pending)
	}
	for w := 0; w < workers; w++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for idx := range jobs {
				result, err := e.renderTime(ctx, idx, true)
				rendered[idx], errs[idx] = result, err
			}
		}()
	}
	for _, idx := range pending {
		select {
		case jobs <- idx:
		case <-ctx.Done():
			close(jobs)
			wg.Wait()
			return nil, ctx.Err()
		}
	}
	close(jobs)
	wg.Wait()
	for i := range e.scene.SampleTimes {
		if errs[i] != nil {
			return nil, errs[i]
		}
		if rendered[i] == nil {
			continue
		}
		if err := e.commitResult(ctx, rendered[i]); err != nil {
			return nil, err
		}
	}
	return e.Timeline()
}

// PrimeCache computes and stores stage results without committing sample
// times. It is intended for real-time previews followed by an edit and an
// incremental RenderAll.
func (e *Engine) PrimeCache(ctx context.Context) error {
	done := e.beginRender()
	defer done()
	for i := range e.scene.SampleTimes {
		if _, err := e.renderTime(ctx, i, false); err != nil {
			return err
		}
	}
	return nil
}

func (e *Engine) Timeline() (*TimelineResult, error) {
	e.resultsMu.Lock()
	defer e.resultsMu.Unlock()
	keys := make([]int, 0, len(e.results))
	for idx := range e.results {
		keys = append(keys, idx)
	}
	sort.Ints(keys)
	out := &TimelineResult{SceneID: e.scene.ID, Times: make([]*TimeResult, 0, len(keys))}
	for _, idx := range keys {
		out.Times = append(out.Times, e.results[idx].Clone())
	}
	return out, nil
}

func (e *Engine) Progress() (committed, total int) {
	e.resultsMu.Lock()
	defer e.resultsMu.Unlock()
	return len(e.results), len(e.scene.SampleTimes)
}

func (e *Engine) StageState(pathID string, timeIndex int, stage Stage) StageState {
	return e.states.get(timeIndex, pathID, stage)
}

func (e *Engine) Transition(pathID string, timeIndex int, stage Stage, to StageState) error {
	e.mu.Lock()
	defer e.mu.Unlock()
	if e.rendering {
		return &Error{Code: CodeState, Op: "transition", Stage: stage, PathID: pathID, TimeIndex: timeIndex, Keyframe: -1,
			Err: fmt.Errorf("manual transition during active render")}
	}
	return e.states.transition(timeIndex, pathID, stage, to)
}

func (e *Engine) RetryTime(timeIndex int) error {
	done := e.beginRender()
	defer done()
	if timeIndex < 0 || timeIndex >= len(e.scene.SampleTimes) {
		return &Error{Code: CodeParam, Op: "retry", Field: "timeIndex", Keyframe: -1, TimeIndex: timeIndex,
			Err: fmt.Errorf("outside range")}
	}
	found := false
	for _, p := range e.scene.Paths {
		for _, stage := range []Stage{StageFlatten, StageCoverage, StageComposite} {
			if e.states.get(timeIndex, p.ID, stage) == StateFailed {
				found = true
				e.states.resetFailed(timeIndex, p.ID)
			}
		}
	}
	if !found {
		return &Error{Code: CodeState, Op: "retry", TimeIndex: timeIndex, Keyframe: -1,
			Err: fmt.Errorf("time has no failed stage")}
	}
	return nil
}

func (e *Engine) CacheStats() CacheStats { return e.cache.stats() }

func (e *Engine) Dependencies() []Dependency { return e.cache.dependencies() }
