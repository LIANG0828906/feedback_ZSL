package engine

import (
	"context"
	"fmt"
	"sort"
	"sync"

	"retargetlab/internal/fsm"
	"retargetlab/internal/model"
	"retargetlab/internal/solver"
	"retargetlab/internal/store"
)

type Engine struct {
	mu   sync.RWMutex
	opMu sync.Mutex

	source  model.Skeleton
	clip    model.Clip
	target  model.TargetVariant
	mapping model.Mapping
	params  solver.Params
	solve   *solver.Solver

	states    []*fsm.Machine
	committed map[int]solver.FrameResult
	invalid   map[int]*Invalidation
	failures  map[int]string
	windows   []store.Window

	deps        DependencyGraph
	cacheHits   int
	cacheMisses int
	lastRun     RunStats
	lastPool    poolStats
	recovery    store.RecoveryReport

	db              *store.Store
	windowSize      int
	maxWorkers      int
	queuePerWorker  int
	bufferPerWorker int
	fault           func(frame int) error
}

func New(cfg Config) (*Engine, error) {
	if cfg.Params.AlgorithmVersion == "" {
		cfg.Params = solver.DefaultParams()
	}
	if cfg.WindowSize < 0 || cfg.MaxWorkers < 0 || cfg.QueuePerWorker < 0 || cfg.BufferPerWorker < 0 {
		return nil, model.Err(model.E_PARAM, "engine_new", "worker, queue, buffer and window limits must not be negative")
	}
	if cfg.WindowSize == 0 {
		cfg.WindowSize = 4
	}
	if cfg.MaxWorkers == 0 {
		cfg.MaxWorkers = 4
	}
	if cfg.QueuePerWorker == 0 {
		cfg.QueuePerWorker = 8
	}
	if cfg.BufferPerWorker == 0 {
		cfg.BufferPerWorker = 2
	}
	if cfg.MaxWorkers > 64 {
		return nil, model.Err(model.E_PARAM, "engine_new", "max workers cannot exceed 64")
	}
	if cfg.QueuePerWorker > 4096 || cfg.BufferPerWorker > 4096 {
		return nil, model.Err(model.E_PARAM, "engine_new", "per-worker queue and buffer limits cannot exceed 4096")
	}
	if cfg.Target.Name == "" {
		cfg.Target.Name = cfg.Target.Skeleton.Name
	}
	sv, err := solver.New(cfg.Source, cfg.Clip, cfg.Target.Skeleton, cfg.Mapping, cfg.Params)
	if err != nil {
		return nil, err
	}
	states := make([]*fsm.Machine, len(cfg.Clip.Frames))
	for i := range states {
		states[i] = fsm.New()
	}
	return &Engine{
		source: cfg.Source, clip: cfg.Clip, target: cfg.Target,
		mapping: cfg.Mapping.Clone(), params: cfg.Params, solve: sv,
		states: states, committed: make(map[int]solver.FrameResult),
		invalid: make(map[int]*Invalidation), failures: make(map[int]string),
		deps: buildDependencyGraph(cfg.Target.Skeleton, cfg.Clip, cfg.Mapping),
		db:   cfg.Store, windowSize: cfg.WindowSize, maxWorkers: cfg.MaxWorkers,
		queuePerWorker: cfg.QueuePerWorker, bufferPerWorker: cfg.BufferPerWorker,
		fault: cfg.FrameFault,
	}, nil
}

func (e *Engine) Recover(ctx context.Context) (store.RecoveryReport, error) {
	e.opMu.Lock()
	defer e.opMu.Unlock()
	if e.db == nil {
		return store.RecoveryReport{}, nil
	}
	expected := store.Expected{
		SkeletonHash:     model.CombinedSkeletonHash(e.source, e.target.Skeleton),
		ParamVersion:     e.params.ParamVersion,
		AlgorithmVersion: e.params.AlgorithmVersion,
	}
	windows, report, err := e.db.LoadAll(ctx, expected)
	if err != nil {
		return report, err
	}
	e.mu.Lock()
	defer e.mu.Unlock()
	e.recovery = report
	e.windows = nil
	e.committed = make(map[int]solver.FrameResult)
	e.invalid = make(map[int]*Invalidation)
	e.failures = make(map[int]string)
	for i := range e.states {
		e.states[i] = fsm.New()
	}
	if report.Invalidated {
		return report, nil
	}
	for _, w := range windows {
		for _, result := range w.Results {
			if result.Frame < 0 || result.Frame >= len(e.states) {
				_ = e.db.WipeAll(ctx)
				return store.RecoveryReport{Invalidated: true, Reason: "checkpoint frame is out of range"}, nil
			}
			e.committed[result.Frame] = result
			e.states[result.Frame].Force(fsm.Committed)
		}
		e.windows = append(e.windows, w)
	}
	return report, nil
}

func (e *Engine) SolveAll(ctx context.Context) (RunStats, error) {
	e.opMu.Lock()
	defer e.opMu.Unlock()
	frames := make([]int, len(e.clip.Frames))
	for i := range frames {
		frames[i] = i
	}
	return e.processFrames(ctx, frames, false)
}

func (e *Engine) SolveRange(ctx context.Context, start, end int) (RunStats, error) {
	if start < 0 || end < start || end >= len(e.clip.Frames) {
		return RunStats{}, model.ErrFrame(model.E_FRAME, "solve_range", start, "", "invalid frame range")
	}
	e.opMu.Lock()
	defer e.opMu.Unlock()
	frames := make([]int, 0, end-start+1)
	for f := start; f <= end; f++ {
		frames = append(frames, f)
	}
	return e.processFrames(ctx, frames, false)
}

func (e *Engine) RetryFailed(ctx context.Context) (RunStats, error) {
	e.opMu.Lock()
	defer e.opMu.Unlock()
	e.mu.RLock()
	var frames []int
	for f := range e.clip.Frames {
		if e.states[f].State() == fsm.Failed {
			frames = append(frames, f)
		}
	}
	e.mu.RUnlock()
	return e.processFrames(ctx, frames, true)
}

func (e *Engine) RetryFrame(ctx context.Context, frame int) (RunStats, error) {
	if frame < 0 || frame >= len(e.clip.Frames) {
		return RunStats{}, model.ErrFrame(model.E_FRAME, "retry_frame", frame, "", "frame is out of range")
	}
	e.opMu.Lock()
	defer e.opMu.Unlock()
	e.mu.RLock()
	state := e.states[frame].State()
	e.mu.RUnlock()
	if state != fsm.Failed {
		return RunStats{}, model.ErrFrame(model.E_STATE, "retry_frame", frame, "", "only failed frames can be retried")
	}
	return e.processFrames(ctx, []int{frame}, true)
}

func (e *Engine) processFrames(ctx context.Context, requested []int, allowFailed bool) (RunStats, error) {
	run := RunStats{}
	requested = uniqueInts(requested)
	var toSolve []int
	hits := 0
	e.mu.RLock()
	for _, frame := range requested {
		if frame < 0 || frame >= len(e.states) {
			e.mu.RUnlock()
			return run, model.ErrFrame(model.E_FRAME, "process_frames", frame, "", "frame is out of range")
		}
		switch e.states[frame].State() {
		case fsm.Committed:
			hits++
		case fsm.Pending:
			toSolve = append(toSolve, frame)
		case fsm.Failed:
			if allowFailed {
				toSolve = append(toSolve, frame)
			}
		case fsm.Solving:
			e.mu.RUnlock()
			return run, model.ErrFrame(model.E_STATE, "process_frames", frame, "", "frame is already solving")
		}
	}
	e.mu.RUnlock()
	run.CacheHits = hits
	run.CacheMisses = len(toSolve)

	for _, chunk := range chunkRuns(toSolve, e.windowSize) {
		if err := e.beginFrames(chunk); err != nil {
			return run, err
		}
		solveFn := func(ctx context.Context, frame int) (solver.FrameResult, error) {
			if e.fault != nil {
				if err := e.fault(frame); err != nil {
					return solver.FrameResult{}, &model.CodedError{
						Code: model.E_FRAME, Op: "injected_frame_fault", Frame: frame, HasFrame: true,
						Message: err.Error(), Cause: err,
					}
				}
			}
			e.mu.RLock()
			sv := e.solve
			e.mu.RUnlock()
			return sv.SolveFrame(ctx, frame)
		}
		outcomes, pool, err := e.solveWithPool(ctx, solveFn, chunk)
		e.mu.Lock()
		e.lastPool = pool
		e.mu.Unlock()
		if err != nil {
			e.failFrames(chunk, err.Error())
			return run, err
		}

		resultByFrame := make(map[int]solver.FrameResult, len(outcomes))
		for _, out := range outcomes {
			if out.err != nil {
				e.failOne(out.frame, out.err.Error())
				run.FailedFrames = append(run.FailedFrames, out.frame)
				continue
			}
			resultByFrame[out.frame] = out.result
			run.FramesSolved++
		}
		for _, segment := range successSegments(chunk, resultByFrame) {
			results := make([]solver.FrameResult, 0, len(segment))
			for _, frame := range segment {
				results = append(results, resultByFrame[frame])
			}
			if _, err := e.commitSegment(ctx, results); err != nil {
				e.failFrames(chunk, err.Error())
				return run, err
			}
		}
	}
	e.mu.Lock()
	e.cacheHits += hits
	e.cacheMisses += len(toSolve)
	e.lastRun = run
	e.mu.Unlock()
	return run, nil
}

func (e *Engine) beginFrames(frames []int) error {
	e.mu.Lock()
	defer e.mu.Unlock()
	for _, frame := range frames {
		state := e.states[frame].State()
		if state != fsm.Pending && state != fsm.Failed {
			return model.ErrFrame(model.E_STATE, "begin_frames", frame, "", fmt.Sprintf("cannot begin solve from state %s", state))
		}
	}
	for _, frame := range frames {
		if err := e.states[frame].Transition(fsm.Solving); err != nil {
			return model.ErrFrame(model.E_STATE, "begin_frames", frame, "", err.Error())
		}
	}
	return nil
}

func (e *Engine) failOne(frame int, message string) {
	e.mu.Lock()
	defer e.mu.Unlock()
	if e.states[frame].State() == fsm.Solving {
		_ = e.states[frame].Transition(fsm.Failed)
		e.failures[frame] = message
	}
}

func (e *Engine) failFrames(frames []int, message string) {
	e.mu.Lock()
	defer e.mu.Unlock()
	for _, frame := range frames {
		if e.states[frame].State() == fsm.Solving {
			_ = e.states[frame].Transition(fsm.Failed)
			e.failures[frame] = message
		}
	}
}

func (e *Engine) commitSegment(ctx context.Context, results []solver.FrameResult) (store.Window, error) {
	if len(results) == 0 {
		return store.Window{}, nil
	}
	sort.Slice(results, func(i, j int) bool { return results[i].Frame < results[j].Frame })
	for i := 1; i < len(results); i++ {
		if results[i].Frame != results[i-1].Frame+1 {
			return store.Window{}, model.ErrFrame(model.E_FRAME, "commit_segment", results[i].Frame, "", "commit segment is not contiguous")
		}
	}
	first := results[0].Snapshot
	for _, r := range results[1:] {
		if r.Snapshot.SkeletonHash != first.SkeletonHash ||
			r.Snapshot.MappingHash != first.MappingHash ||
			r.Snapshot.ParamVersion != first.ParamVersion ||
			r.Snapshot.AlgorithmVersion != first.AlgorithmVersion {
			return store.Window{}, model.ErrFrame(model.E_STATE, "commit_segment", r.Frame, "", "frame snapshots differ inside commit window")
		}
	}
	scale := make(map[string]float64, len(first.ScaleCoefficients))
	for k, v := range first.ScaleCoefficients {
		scale[k] = v
	}
	w := store.Window{
		StartFrame: results[0].Frame, EndFrame: results[len(results)-1].Frame,
		SkeletonHash: first.SkeletonHash, MappingHash: first.MappingHash,
		ParamVersion: first.ParamVersion, AlgorithmVersion: first.AlgorithmVersion,
		ScaleValues: scale, Mapping: first.Mapping.Clone(), Results: results,
	}
	e.mu.Lock()
	for _, r := range results {
		if e.states[r.Frame].State() != fsm.Solving {
			e.mu.Unlock()
			return store.Window{}, model.ErrFrame(model.E_STATE, "commit_segment", r.Frame, "", "cannot commit a frame that is not solving")
		}
	}
	var nextSeq int64 = 1
	for _, existing := range e.windows {
		if existing.CommitSeq >= nextSeq {
			nextSeq = existing.CommitSeq + 1
		}
	}
	e.mu.Unlock()

	if e.db != nil {
		saved, err := e.db.SaveWindow(ctx, w)
		if err != nil {
			return store.Window{}, err
		}
		w = saved
	} else {
		w.WindowID = w.StartFrame
		w.CommitSeq = nextSeq
		w.ContentHash = store.ComputeContentHash(w)
	}

	e.mu.Lock()
	defer e.mu.Unlock()
	for _, r := range results {
		if err := e.states[r.Frame].Transition(fsm.Committed); err != nil {
			return store.Window{}, model.ErrFrame(model.E_STATE, "commit_segment", r.Frame, "", err.Error())
		}
		e.committed[r.Frame] = r
		delete(e.invalid, r.Frame)
		delete(e.failures, r.Frame)
	}
	e.windows = append(e.windows, w)
	return w, nil
}

func (e *Engine) ApplyMappingRule(index int, replacement model.JointRule) (InvalidationReport, error) {
	e.opMu.Lock()
	defer e.opMu.Unlock()
	e.mu.RLock()
	if index < 0 || index >= len(e.mapping.Rules) {
		e.mu.RUnlock()
		return InvalidationReport{}, model.Err(model.E_PARAM, "edit_mapping", "mapping rule index is out of range")
	}
	oldMapping := e.mapping.Clone()
	e.mu.RUnlock()
	newMapping := oldMapping.Clone()
	newMapping.Rules[index] = cloneRule(replacement)
	newSolver, err := solver.New(e.source, e.clip, e.target.Skeleton, newMapping, e.params)
	if err != nil {
		return InvalidationReport{}, err
	}
	e.mu.RLock()
	oldSolver := e.solve
	committedCount := len(e.committed)
	e.mu.RUnlock()
	entries := e.mappingEditInvalidations(oldSolver, newSolver, oldMapping, newMapping, index)

	e.mu.Lock()
	e.mapping = newMapping
	e.solve = newSolver
	e.deps = buildDependencyGraph(e.target.Skeleton, e.clip, newMapping)
	e.mu.Unlock()
	e.mergeInvalidations(entries)
	e.mu.Lock()
	e.cacheHits += committedCount
	e.mu.Unlock()
	return buildInvalidationReport(entries, committedCount), nil
}

func (e *Engine) SetScale(key string, value float64) (InvalidationReport, error) {
	e.opMu.Lock()
	defer e.opMu.Unlock()
	e.mu.RLock()
	oldSolver := e.solve
	oldParams := cloneParams(e.params)
	committedCount := len(e.committed)
	coeff, found := scaleCoefficient(e.mapping, key)
	e.mu.RUnlock()
	if !found {
		return InvalidationReport{}, model.Err(model.E_PARAM, "edit_scale", fmt.Sprintf("unknown scale coefficient %q", key))
	}
	newParams := cloneParams(oldParams)
	if newParams.ScaleOverrides == nil {
		newParams.ScaleOverrides = make(map[string]float64)
	}
	newParams.ScaleOverrides[key] = value
	newSolver, err := solver.New(e.source, e.clip, e.target.Skeleton, e.mapping, newParams)
	if err != nil {
		return InvalidationReport{}, err
	}
	entries := e.scaleEditInvalidations(oldSolver, newSolver, coeff)
	e.mu.Lock()
	e.params = newParams
	e.solve = newSolver
	e.mu.Unlock()
	e.mergeInvalidations(entries)
	e.mu.Lock()
	e.cacheHits += committedCount
	e.mu.Unlock()
	return buildInvalidationReport(entries, committedCount), nil
}

func (e *Engine) StateSnapshot() StateSnapshot {
	e.mu.RLock()
	defer e.mu.RUnlock()
	snap := StateSnapshot{
		TargetName: e.target.Name, Source: e.source, Target: e.target.Skeleton,
		Clip: e.clip, Mapping: e.mapping.Clone(), ScaleValues: e.solve.ScaleValues(),
		ParamVersion: e.params.ParamVersion, AlgorithmVersion: e.params.AlgorithmVersion,
		TotalFrames: len(e.clip.Frames), Windows: make([]WindowView, 0, len(e.windows)),
		Dependencies: e.deps, Invalidations: e.invalidListLocked(),
		CacheHits: e.cacheHits, CacheMisses: e.cacheMisses, LastRun: e.lastRun,
		Recovery: e.recovery, Pool: e.lastPool,
	}
	if e.db != nil {
		snap.StorePath = e.db.Path()
	}
	for _, w := range e.windows {
		snap.Windows = append(snap.Windows, WindowView{
			StartFrame: w.StartFrame, EndFrame: w.EndFrame, MappingHash: w.MappingHash,
			ContentHash: w.ContentHash, CommitSeq: w.CommitSeq, ParamVersion: w.ParamVersion,
		})
	}
	for f := range e.states {
		state := e.states[f].State()
		view := FrameView{Frame: f, State: state, CacheHit: state == fsm.Committed}
		switch state {
		case fsm.Committed:
			snap.CommittedFrames++
		case fsm.Pending:
			snap.PendingFrames++
		case fsm.Solving:
			snap.SolvingFrames++
		case fsm.Failed:
			snap.FailedFrames++
		}
		if inv, ok := e.invalid[f]; ok {
			view.Invalidated = true
			view.InvalidJoints = append([]string(nil), inv.Joints...)
			view.InvalidReasons = append([]string(nil), inv.Reasons...)
		}
		if msg, ok := e.failures[f]; ok {
			view.Error = msg
		}
		if r, ok := e.committed[f]; ok {
			root := r.RootTrajectory
			shift := r.RootShift
			cs := r.Constraint
			view.Root = &root
			view.RootShift = &shift
			view.Contacts = append([]string(nil), r.Contacts...)
			view.Constraint = &cs
			view.MappingHash = r.Snapshot.MappingHash
			view.ScaleValues = cloneFloatMap(r.Snapshot.ScaleCoefficients)
		}
		snap.Frames = append(snap.Frames, view)
	}
	if snap.TotalFrames > 0 {
		snap.Progress = float64(snap.CommittedFrames) / float64(snap.TotalFrames)
	}
	return snap
}

func (e *Engine) FrameDetail(frame int) (FrameDetail, error) {
	e.mu.RLock()
	defer e.mu.RUnlock()
	if frame < 0 || frame >= len(e.states) {
		return FrameDetail{}, model.ErrFrame(model.E_FRAME, "frame_detail", frame, "", "frame is out of range")
	}
	view := FrameView{Frame: frame, State: e.states[frame].State()}
	view.CacheHit = view.State == fsm.Committed
	if inv, ok := e.invalid[frame]; ok {
		view.Invalidated = true
		view.InvalidJoints = append([]string(nil), inv.Joints...)
		view.InvalidReasons = append([]string(nil), inv.Reasons...)
	}
	view.Error = e.failures[frame]
	var result *solver.FrameResult
	if r, ok := e.committed[frame]; ok {
		copyResult := r
		result = &copyResult
		root := r.RootTrajectory
		shift := r.RootShift
		cs := r.Constraint
		view.Root = &root
		view.RootShift = &shift
		view.Contacts = append([]string(nil), r.Contacts...)
		view.Constraint = &cs
		view.MappingHash = r.Snapshot.MappingHash
		view.ScaleValues = cloneFloatMap(r.Snapshot.ScaleCoefficients)
	}
	fd := FrameDependency{Frame: frame}
	if frame < len(e.deps.Frames) {
		fd = e.deps.Frames[frame]
	}
	var inv *Invalidation
	if existing, ok := e.invalid[frame]; ok {
		copyInv := *existing
		inv = &copyInv
	}
	return FrameDetail{Frame: view, Result: result, Dependency: fd, Invalidation: inv}, nil
}

func (e *Engine) State(frame int) (fsm.FrameState, error) {
	e.mu.RLock()
	defer e.mu.RUnlock()
	if frame < 0 || frame >= len(e.states) {
		return "", model.ErrFrame(model.E_FRAME, "frame_state", frame, "", "frame is out of range")
	}
	return e.states[frame].State(), nil
}

func (e *Engine) Transition(frame int, to fsm.FrameState) error {
	e.mu.Lock()
	defer e.mu.Unlock()
	if frame < 0 || frame >= len(e.states) {
		return model.ErrFrame(model.E_FRAME, "transition_frame", frame, "", "frame is out of range")
	}
	if err := e.states[frame].Transition(to); err != nil {
		return model.ErrFrame(model.E_STATE, "transition_frame", frame, "", err.Error())
	}
	return nil
}

func (e *Engine) CommittedResults() map[int]solver.FrameResult {
	e.mu.RLock()
	defer e.mu.RUnlock()
	out := make(map[int]solver.FrameResult, len(e.committed))
	for k, v := range e.committed {
		out[k] = v
	}
	return out
}

func (e *Engine) Progress() (committed, total int) {
	e.mu.RLock()
	defer e.mu.RUnlock()
	return len(e.committed), len(e.states)
}

func (e *Engine) Mapping() model.Mapping {
	e.mu.RLock()
	defer e.mu.RUnlock()
	return e.mapping.Clone()
}

func (e *Engine) Params() solver.Params {
	e.mu.RLock()
	defer e.mu.RUnlock()
	return cloneParams(e.params)
}

func chunkRuns(frames []int, size int) [][]int {
	if len(frames) == 0 {
		return nil
	}
	if size < 1 {
		size = 1
	}
	var out [][]int
	run := []int{frames[0]}
	for _, f := range frames[1:] {
		if f != run[len(run)-1]+1 || len(run) >= size {
			out = append(out, run)
			run = []int{f}
			continue
		}
		run = append(run, f)
	}
	out = append(out, run)
	return out
}

func successSegments(frames []int, results map[int]solver.FrameResult) [][]int {
	var out [][]int
	var current []int
	for _, f := range frames {
		if _, ok := results[f]; ok {
			current = append(current, f)
		} else if len(current) > 0 {
			out = append(out, current)
			current = nil
		}
	}
	if len(current) > 0 {
		out = append(out, current)
	}
	return out
}

func buildInvalidationReport(entries []Invalidation, hits int) InvalidationReport {
	report := InvalidationReport{Entries: entries, Frames: len(entries), CacheHits: hits}
	joints := make([]string, 0)
	for _, e := range entries {
		joints = append(joints, e.Joints...)
	}
	report.Joints = uniqueSorted(joints)
	return report
}

func cloneRule(r model.JointRule) model.JointRule {
	r.Targets = append([]string(nil), r.Targets...)
	r.Phases = append([]int(nil), r.Phases...)
	return r
}

func scaleCoefficient(m model.Mapping, key string) (model.ScaleCoefficient, bool) {
	for _, s := range m.ScaleDefaults {
		if s.Key == key {
			return s, true
		}
	}
	return model.ScaleCoefficient{}, false
}

func cloneParams(p solver.Params) solver.Params {
	out := p
	if p.ScaleOverrides != nil {
		out.ScaleOverrides = make(map[string]float64, len(p.ScaleOverrides))
		for k, v := range p.ScaleOverrides {
			out.ScaleOverrides[k] = v
		}
	}
	return out
}

func cloneFloatMap(in map[string]float64) map[string]float64 {
	if in == nil {
		return nil
	}
	out := make(map[string]float64, len(in))
	for k, v := range in {
		out[k] = v
	}
	return out
}
