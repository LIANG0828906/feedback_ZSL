package pipeline

import (
	"context"
	"fmt"
	"sort"
	"strings"
	"sync"
)

const (
	PathReasonCumulative = "P_CUMULATIVE_RATIO_GT_THRESHOLD"
	PathReasonRescan     = "P_RESCAN_RATIO_LE_THRESHOLD"
)

// RunOptions controls execution but never window membership or semantics.
type RunOptions struct {
	Workers                   int
	MaxWorkers                int
	WindowBufferPerFeature    int
	MaxWindowBufferPerFeature int
	RatioThreshold            int64
	CommitEvery               int
	Checkpoint                CheckpointBackend
	Resume                    bool
}

// DefaultRunOptions returns deterministic small-machine defaults.
func DefaultRunOptions() RunOptions {
	return RunOptions{
		Workers: 4, MaxWorkers: 64,
		WindowBufferPerFeature: 64, MaxWindowBufferPerFeature: 1024,
		RatioThreshold: 3, CommitEvery: 8, Resume: true,
	}
}

// Engine executes the streaming kernel.
type Engine struct{}

// Run loads a compatible checkpoint when configured, processes uncommitted
// arrival batches in parallel across features, and commits state before
// advancing the global event sequence.
func (Engine) Run(ctx context.Context, p *Project, opts RunOptions) (*RunResult, error) {
	if p == nil {
		return nil, NewBusinessError(CodeSchemaUnsupported, "nil project")
	}
	opts = normalizeRunOptions(opts, len(p.Features))
	if err := validateRunOptions(opts); err != nil {
		return nil, err
	}
	if err := validateProjectForRun(p); err != nil {
		return nil, err
	}
	plans, err := WindowPlans(p)
	if err != nil {
		return nil, err
	}
	run := newRunState(p, plans, opts)

	if err := validateGlobalWatermark(p); err != nil {
		return nil, err
	}
	fingerprint, err := ProjectFingerprint(p)
	if err != nil {
		return nil, err
	}
	if opts.Checkpoint != nil {
		if !opts.Resume {
			if err := opts.Checkpoint.Reset(); err != nil {
				return nil, checkpointError("reset", err)
			}
		} else {
			snapshot, info, err := opts.Checkpoint.Load(p, fingerprint)
			if err != nil {
				return nil, err
			}
			if info.FullRebuild {
				if err := opts.Checkpoint.Reset(); err != nil {
					return nil, checkpointError("reset_after_version_mismatch", err)
				}
				run.result.CheckpointFallback = true
				run.result.CheckpointNote = info.Note
			} else if info.Found && snapshot != nil {
				if err := run.restore(p, snapshot); err != nil {
					return nil, err
				}
				run.result.ResumedFromSeq = snapshot.LastEventSeq
				run.result.CheckpointNote = info.Note
			}
		}
	}

	remaining := p.Events[int(run.lastSeq):]
	for len(remaining) > 0 {
		batchSize := opts.CommitEvery
		if batchSize > len(remaining) {
			batchSize = len(remaining)
		}
		batch := remaining[:batchSize]
		if err := run.processBatch(batch, opts); err != nil {
			run.finishPartial()
			return run.result, err
		}
		run.lastSeq = batch[len(batch)-1].Seq
		if p.WatermarkEnabled {
			if wm := batch[len(batch)-1].WatermarkMs; wm != nil {
				value := *wm
				run.watermark = &value
			}
		}
		if err := run.persist(opts, fingerprint, false); err != nil {
			return nil, err
		}
		run.result.LastCommittedSeq = run.lastSeq
		remaining = remaining[batchSize:]
		if ctx.Err() != nil {
			run.finishPartial()
			run.result.Interrupted = true
			return run.result, NewBusinessError(CodeInterrupted, "run interrupted after complete checkpoint").
				WithContext("last_committed_seq", fmt.Sprint(run.lastSeq)).
				WithContext("checkpoint_complete", "true")
		}
	}
	run.commitAll()
	if err := run.persist(opts, fingerprint, true); err != nil {
		return nil, err
	}
	run.result.LastCommittedSeq = run.lastSeq
	run.finalizeResult(true)
	return run.result, nil
}

func validateRunOptions(opts RunOptions) error {
	if opts.Workers < 1 {
		return NewBusinessError(CodeInvalidWorkers, "worker count must be at least one").
			WithContext("workers", fmt.Sprint(opts.Workers))
	}
	if opts.MaxWorkers < 1 {
		return NewBusinessError(CodeInvalidWorkers, "maximum worker budget must be at least one").
			WithContext("max_workers", fmt.Sprint(opts.MaxWorkers))
	}
	if opts.Workers > opts.MaxWorkers {
		return NewBusinessError(CodeThreadBudgetExceeded, "requested workers exceed the thread budget").
			WithContext("workers", fmt.Sprint(opts.Workers)).
			WithContext("max_workers", fmt.Sprint(opts.MaxWorkers))
	}
	if opts.WindowBufferPerFeature < 1 {
		return NewBusinessError(CodeInvalidWindowBuffer, "window buffer must be at least one").
			WithContext("buffer", fmt.Sprint(opts.WindowBufferPerFeature))
	}
	if opts.MaxWindowBufferPerFeature < 1 {
		return NewBusinessError(CodeInvalidWindowBuffer, "maximum window buffer must be at least one").
			WithContext("max_buffer", fmt.Sprint(opts.MaxWindowBufferPerFeature))
	}
	if opts.WindowBufferPerFeature > opts.MaxWindowBufferPerFeature {
		return NewBusinessError(CodeBufferBudgetExceeded, "requested window buffer exceeds the memory budget").
			WithContext("buffer", fmt.Sprint(opts.WindowBufferPerFeature)).
			WithContext("max_buffer", fmt.Sprint(opts.MaxWindowBufferPerFeature))
	}
	if opts.CommitEvery < 1 {
		return NewBusinessError(CodeInvalidWindowBuffer, "commit batch size must be at least one").
			WithContext("commit_every", fmt.Sprint(opts.CommitEvery))
	}
	if opts.RatioThreshold <= 0 {
		return NewBusinessError(CodeFeatureWidthBelowStep, "path threshold must be positive").
			WithContext("threshold", fmt.Sprint(opts.RatioThreshold))
	}
	return nil
}

// Run is the package-level convenience entry point.
func Run(ctx context.Context, p *Project, opts RunOptions) (*RunResult, error) {
	return (Engine{}).Run(ctx, p, opts)
}

func normalizeRunOptions(opts RunOptions, featureCount int) RunOptions {
	defaults := DefaultRunOptions()
	if opts.MaxWorkers == 0 {
		opts.MaxWorkers = defaults.MaxWorkers
	}
	if opts.Workers == 0 {
		opts.Workers = featureCount
		if opts.Workers > defaults.Workers {
			opts.Workers = defaults.Workers
		}
	}
	if opts.MaxWindowBufferPerFeature == 0 {
		opts.MaxWindowBufferPerFeature = defaults.MaxWindowBufferPerFeature
	}
	if opts.WindowBufferPerFeature == 0 {
		opts.WindowBufferPerFeature = defaults.WindowBufferPerFeature
	}
	if opts.RatioThreshold == 0 {
		opts.RatioThreshold = defaults.RatioThreshold
	}
	if opts.CommitEvery == 0 {
		opts.CommitEvery = defaults.CommitEvery
	}
	return opts
}

func validateProjectForRun(p *Project) error {
	if len(p.Events) == 0 {
		return NewBusinessError(CodeEventSequenceInvalid, "event stream is empty").
			WithContext("project_id", p.ProjectID)
	}
	seen := map[string]bool{}
	for _, f := range p.Features {
		if seen[f.ID] {
			return NewBusinessError(CodeFeatureDuplicate, "duplicate feature %q", f.ID).
				WithContext("feature_id", f.ID)
		}
		seen[f.ID] = true
		if f.WindowWidthMs <= 0 {
			return NewBusinessError(CodeFeatureWindowWidth, "window width must be positive").
				WithContext("feature_id", f.ID).WithContext("value", fmt.Sprint(f.WindowWidthMs))
		}
		if f.WindowStepMs <= 0 {
			return NewBusinessError(CodeFeatureStepZero, "window step must be positive").
				WithContext("feature_id", f.ID).WithContext("value", fmt.Sprint(f.WindowStepMs))
		}
		if f.WindowWidthMs < f.WindowStepMs {
			return NewBusinessError(CodeFeatureWidthBelowStep, "window width must be at least the step").
				WithContext("feature_id", f.ID)
		}
		if f.AllowedLatenessMs < 0 {
			return NewBusinessError(CodeFeatureStepZero, "allowed lateness must not be negative").
				WithContext("feature_id", f.ID).WithContext("value", fmt.Sprint(f.AllowedLatenessMs))
		}
		if len(f.Aggregations) == 0 {
			return NewBusinessError(CodeSchemaUnsupported, "feature needs at least one aggregate").
				WithContext("feature_id", f.ID)
		}
		for i, agg := range f.Aggregations {
			if !agg.Kind.Valid() {
				return NewBusinessError(CodeAggFunctionUnknown, "unknown aggregate function %q", agg.Kind).
					WithContext("feature_id", f.ID).WithContext("aggregate_index", fmt.Sprint(i))
			}
			if agg.Weight <= 0 || agg.Weight > NormalizedWeightScale {
				return NewBusinessError(CodeAggWeightOutOfRange, "aggregate weight is out of range").
					WithContext("feature_id", f.ID).WithContext("aggregate_index", fmt.Sprint(i)).
					WithContext("weight", fmt.Sprint(agg.Weight)).
					WithContext("valid_range", "1..1000000")
			}
		}
	}
	for i, e := range p.Events {
		if e.Seq != int64(i+1) {
			return NewBusinessError(CodeEventSequenceInvalid, "event sequence must be contiguous").
				WithContext("event_seq", fmt.Sprint(e.Seq)).
				WithContext("want", fmt.Sprint(i+1))
		}
		if !seen[e.FeatureID] {
			return NewBusinessError(CodeEventUnknownFeature, "unknown feature %q", e.FeatureID).
				WithContext("event_seq", fmt.Sprint(e.Seq))
		}
	}
	return nil
}

func validateGlobalWatermark(p *Project) error {
	if !p.WatermarkEnabled {
		return nil
	}
	var previous int64
	for i, e := range p.Events {
		if e.WatermarkMs == nil {
			return NewBusinessError(CodeWatermarkMissing, "watermark is missing").
				WithContext("event_seq", fmt.Sprint(e.Seq)).WithContext("feature_id", e.FeatureID)
		}
		if i > 0 && *e.WatermarkMs < previous {
			return NewBusinessError(CodeWatermarkBeforeCommitted, "watermark moved earlier than a committed checkpoint").
				WithContext("event_seq", fmt.Sprint(e.Seq)).
				WithContext("watermark", fmt.Sprint(*e.WatermarkMs)).
				WithContext("previous_watermark", fmt.Sprint(previous)).
				WithContext("reason", CodeWatermarkRegression)
		}
		previous = *e.WatermarkMs
	}
	return nil
}

type runState struct {
	project   *Project
	opts      RunOptions
	features  map[string]*featureRuntime
	lastSeq   int64
	watermark *int64
	result    *RunResult
}

type featureRuntime struct {
	feature  FeatureDefinition
	plan     FeatureWindowPlan
	events   []eventRef
	mode     TraversalMode
	path     PathDecision
	windows  map[int64]*windowRuntime
	active   map[int64]bool
	discards []DiscardRecord
}

type windowRuntime struct {
	key            WindowKey
	accepted       map[int64]bool
	refs           []eventRef
	committed      bool
	recomputeCount int64
	acc            *windowAccumulator
}

func newRunState(p *Project, plans map[string]FeatureWindowPlan, opts RunOptions) *runState {
	run := &runState{
		project: p, opts: opts,
		features: make(map[string]*featureRuntime, len(p.Features)),
		result: &RunResult{
			ProjectID: p.ProjectID, SchemaVersion: p.SchemaVersion,
			WatermarkEnabled: p.WatermarkEnabled, WatermarkApplied: p.WatermarkEnabled,
			BufferLimit: opts.WindowBufferPerFeature,
			WorkersUsed: minInt(opts.Workers, len(p.Features)),
		},
	}
	if !p.WatermarkEnabled {
		run.result.WatermarkNote = "disabled_by_declaration"
	} else {
		run.result.WatermarkNote = "enabled"
	}
	byFeature := make(map[string][]eventRef, len(p.Features))
	for _, e := range p.Events {
		byFeature[e.FeatureID] = append(byFeature[e.FeatureID], eventRef{
			Seq: e.Seq, ObservedAtMs: e.ObservedAtMs, Value: e.Value,
		})
	}
	for _, f := range p.Features {
		mode := TraversalRescan
		reason := PathReasonRescan
		if f.WindowWidthMs > opts.RatioThreshold*f.WindowStepMs {
			mode = TraversalCumulative
			reason = PathReasonCumulative
		}
		decision := PathDecision{
			FeatureID: f.ID, WidthMs: f.WindowWidthMs, StepMs: f.WindowStepMs,
			RatioNum: f.WindowWidthMs, RatioDen: f.WindowStepMs,
			Threshold: opts.RatioThreshold, Mode: mode, ReasonCode: reason,
		}
		run.features[f.ID] = &featureRuntime{
			feature: f, plan: plans[f.ID], events: byFeature[f.ID],
			mode: mode, path: decision,
			windows: map[int64]*windowRuntime{}, active: map[int64]bool{},
		}
		run.result.PathDecisions = append(run.result.PathDecisions, decision)
	}
	sort.Slice(run.result.PathDecisions, func(i, j int) bool {
		return run.result.PathDecisions[i].FeatureID < run.result.PathDecisions[j].FeatureID
	})
	return run
}

func (r *runState) restore(p *Project, snapshot *CheckpointSnapshot) error {
	if snapshot.ProjectFingerprint != "" {
		fp, err := ProjectFingerprint(p)
		if err != nil {
			return err
		}
		if snapshot.ProjectFingerprint != fp {
			return NewBusinessError(CodeCheckpointVersionMismatch, "checkpoint fingerprint mismatch").
				WithContext("checkpoint", snapshot.ProjectFingerprint).
				WithContext("project", fp)
		}
	}
	if snapshot.LastEventSeq < 0 || snapshot.LastEventSeq > int64(len(p.Events)) {
		return NewBusinessError(CodeCheckpointTruncated, "checkpoint event sequence is outside stream").
			WithContext("last_event_seq", fmt.Sprint(snapshot.LastEventSeq)).
			WithContext("event_count", fmt.Sprint(len(p.Events)))
	}
	if snapshot.WatermarkEnabled != p.WatermarkEnabled {
		return NewBusinessError(CodeCheckpointVersionMismatch, "watermark mode mismatch")
	}
	if p.WatermarkEnabled && snapshot.LastEventSeq > 0 {
		lastEvent := p.Events[snapshot.LastEventSeq-1]
		if lastEvent.WatermarkMs == nil || snapshot.WatermarkMs == nil ||
			*snapshot.WatermarkMs < *lastEvent.WatermarkMs {
			return NewBusinessError(CodeCheckpointTruncated, "checkpoint watermark is inconsistent")
		}
	}
	r.lastSeq = snapshot.LastEventSeq
	if snapshot.WatermarkMs != nil {
		v := *snapshot.WatermarkMs
		r.watermark = &v
	}
	for i := range snapshot.Windows {
		pw := snapshot.Windows[i]
		rt, ok := r.features[pw.Key.FeatureID]
		if !ok {
			return NewBusinessError(CodeCheckpointVersionMismatch, "checkpoint contains unknown feature").
				WithContext("feature_id", pw.Key.FeatureID)
		}
		planned := false
		for _, key := range rt.plan.Windows {
			if key == pw.Key {
				planned = true
				break
			}
		}
		if !planned {
			return NewBusinessError(CodeCheckpointTruncated, "checkpoint window is outside the window plan").
				WithContext("window", pw.Key.String())
		}
		ws, err := rt.restoreWindow(pw, p)
		if err != nil {
			return err
		}
		rt.windows[pw.Key.StartMs] = ws
		if !ws.committed {
			rt.active[pw.Key.StartMs] = true
		}
	}
	for _, d := range snapshot.Discards {
		rt, ok := r.features[d.FeatureID]
		if !ok {
			return NewBusinessError(CodeCheckpointVersionMismatch, "discard references unknown feature").
				WithContext("feature_id", d.FeatureID)
		}
		rt.discards = append(rt.discards, d)
	}
	for _, rt := range r.features {
		if len(rt.active) > r.opts.WindowBufferPerFeature {
			return NewBusinessError(CodeBackpressureWindowBuffer, "restored open window buffer exceeds limit").
				WithContext("feature_id", rt.feature.ID).
				WithContext("active", fmt.Sprint(len(rt.active))).
				WithContext("limit", fmt.Sprint(r.opts.WindowBufferPerFeature))
		}
	}
	return nil
}

func (rt *featureRuntime) restoreWindow(pw PersistedWindow, p *Project) (*windowRuntime, error) {
	ws := &windowRuntime{
		key: pw.Key, accepted: map[int64]bool{}, committed: pw.Status == WindowCommitted,
		recomputeCount: pw.RecomputeCount,
		acc:            newWindowAccumulator(rt.feature.Aggregations),
	}
	for _, seq := range pw.AcceptedSeqs {
		event, ok := p.EventBySeq(seq)
		if !ok {
			return nil, NewBusinessError(CodeCheckpointTruncated, "checkpoint references missing event").
				WithContext("event_seq", fmt.Sprint(seq))
		}
		if event.FeatureID != rt.feature.ID || event.ObservedAtMs < pw.Key.StartMs || event.ObservedAtMs >= pw.Key.EndMs {
			return nil, NewBusinessError(CodeCheckpointTruncated, "checkpoint event is outside restored window").
				WithContext("event_seq", fmt.Sprint(seq)).
				WithContext("window", pw.Key.String())
		}
		ref := eventRef{Seq: event.Seq, ObservedAtMs: event.ObservedAtMs, Value: event.Value}
		ws.accepted[seq] = true
		ws.refs = append(ws.refs, ref)
		ws.acc.add(event.Value)
	}
	sortRefs(ws.refs)
	recomputed := aggregateRefs(ws.refs, rt.feature.Aggregations)
	if len(pw.Aggregates) != len(recomputed) {
		return nil, NewBusinessError(CodeCheckpointTruncated, "checkpoint aggregate set is incomplete").
			WithContext("window", pw.Key.String())
	}
	for i := range recomputed {
		if pw.Aggregates[i] != recomputed[i] {
			return nil, NewBusinessError(CodeCheckpointTruncated, "checkpoint aggregate state is inconsistent").
				WithContext("window", pw.Key.String()).
				WithContext("aggregate", string(recomputed[i].Kind))
		}
	}
	return ws, nil
}

func (r *runState) processBatch(batch []ObservationEvent, opts RunOptions) error {
	grouped := make(map[string][]ObservationEvent, len(r.features))
	for _, e := range batch {
		grouped[e.FeatureID] = append(grouped[e.FeatureID], e)
	}
	type task struct {
		index int
		rt    *featureRuntime
	}
	tasks := make([]task, 0, len(grouped))
	for idx, f := range r.project.Features {
		if events := grouped[f.ID]; len(events) > 0 {
			tasks = append(tasks, task{index: idx, rt: r.features[f.ID]})
		}
	}
	sem := make(chan struct{}, opts.Workers)
	errCh := make(chan indexedError, len(tasks))
	var wg sync.WaitGroup
	for _, t := range tasks {
		task := t
		sem <- struct{}{}
		wg.Add(1)
		go func() {
			defer wg.Done()
			defer func() { <-sem }()
			errCh <- indexedError{index: task.index, err: task.rt.process(grouped[task.rt.feature.ID], r.opts)}
		}()
	}
	wg.Wait()
	close(errCh)
	var selected *indexedError
	for e := range errCh {
		if e.err == nil {
			continue
		}
		if selected == nil || e.index < selected.index {
			value := e
			selected = &value
		}
	}
	if selected != nil {
		return selected.err
	}
	return nil
}

type indexedError struct {
	index int
	err   error
}

func (rt *featureRuntime) process(events []ObservationEvent, opts RunOptions) error {
	for _, event := range events {
		watermark, hasWatermark := event.WatermarkMsValue()
		candidates := candidateStarts(rt.plan, rt.feature.WindowWidthMs, event.ObservedAtMs)
		for _, key := range candidates {
			ws, exists := rt.windows[key.StartMs]
			if !exists {
				closedByWatermark := hasWatermark && watermark >= key.EndMs+rt.feature.AllowedLatenessMs
				if !closedByWatermark && len(rt.active) >= opts.WindowBufferPerFeature {
					return NewBusinessError(CodeBackpressureWindowBuffer, "feature window buffer is exhausted").
						WithContext("feature_id", rt.feature.ID).
						WithContext("window", key.String()).
						WithContext("event_seq", fmt.Sprint(event.Seq)).
						WithContext("active", fmt.Sprint(len(rt.active))).
						WithContext("limit", fmt.Sprint(opts.WindowBufferPerFeature))
				}
				ws = &windowRuntime{
					key: key, accepted: map[int64]bool{},
					committed: closedByWatermark,
					acc:       newWindowAccumulator(rt.feature.Aggregations),
				}
				rt.windows[key.StartMs] = ws
				if !ws.committed {
					rt.active[key.StartMs] = true
				}
			}
			if hasWatermark && (ws.committed || watermark >= key.EndMs+rt.feature.AllowedLatenessMs) {
				ws.committed = true
				delete(rt.active, key.StartMs)
				rt.discards = append(rt.discards, DiscardRecord{
					EventSeq: event.Seq, FeatureID: rt.feature.ID, Window: key,
					ObservedAtMs: event.ObservedAtMs, WatermarkMs: watermark,
					LateByMs:   watermark - event.ObservedAtMs,
					ReasonCode: "E_LATE_BEYOND_ALLOWED_LATENESS",
				})
				continue
			}
			wasLate := hasWatermark && watermark > event.ObservedAtMs
			ref := eventRef{Seq: event.Seq, ObservedAtMs: event.ObservedAtMs, Value: event.Value}
			ws.accepted[event.Seq] = true
			ws.refs = append(ws.refs, ref)
			ws.acc.add(event.Value)
			if wasLate && len(ws.refs) > 1 {
				ws.recomputeCount++
			}
		}
		if hasWatermark {
			for start, active := range rt.active {
				if !active {
					continue
				}
				ws := rt.windows[start]
				if ws != nil && watermark >= ws.key.EndMs+rt.feature.AllowedLatenessMs {
					ws.committed = true
					delete(rt.active, start)
				}
			}
		}
	}
	return nil
}

// WatermarkMsValue returns an event's watermark without exposing nil pointer
// handling to every caller.
func (e ObservationEvent) WatermarkMsValue() (int64, bool) {
	if e.WatermarkMs == nil {
		return 0, false
	}
	return *e.WatermarkMs, true
}

func (r *runState) commitAll() {
	for _, rt := range r.features {
		for start, ws := range rt.windows {
			ws.committed = true
			delete(rt.active, start)
		}
	}
}

func (r *runState) finishPartial() {
	for _, rt := range r.features {
		sortDiscards(rt.discards)
	}
	r.finalizeResult(false)
}

func (r *runState) finalizeResult(final bool) {
	r.result.Windows = r.result.Windows[:0]
	r.result.Discards = r.result.Discards[:0]
	r.result.RecomputeTotal = 0
	var windows []WindowResult
	var discards []DiscardRecord
	for _, f := range r.project.Features {
		rt := r.features[f.ID]
		for _, key := range rt.plan.Windows {
			ws := rt.windows[key.StartMs]
			var result WindowResult
			if ws == nil {
				result = WindowResult{
					Key: key, Status: WindowOpen, Aggregates: aggregateRefs(nil, f.Aggregations),
					Traversal: rt.mode, PathReasonCode: rt.path.ReasonCode,
				}
				if final {
					result.Status = WindowCommitted
				}
			} else {
				result = rt.windowResult(ws, final)
			}
			windows = append(windows, result)
			r.result.RecomputeTotal += result.RecomputeCount
		}
		discards = append(discards, rt.discards...)
	}
	sort.Slice(windows, func(i, j int) bool {
		if windows[i].Key.FeatureID != windows[j].Key.FeatureID {
			return windows[i].Key.FeatureID < windows[j].Key.FeatureID
		}
		return windows[i].Key.StartMs < windows[j].Key.StartMs
	})
	sortDiscards(discards)
	r.result.Windows = windows
	r.result.Discards = discards
}

func (rt *featureRuntime) windowResult(ws *windowRuntime, final bool) WindowResult {
	status := ws.status(final)
	seqs := refsToSeqs(ws.refs)
	var aggregates []AggregateResult
	if rt.mode == TraversalCumulative {
		aggregates = ws.acc.results()
	} else {
		refs := rt.rescan(ws)
		aggregates = aggregateRefs(refs, rt.feature.Aggregations)
		seqs = refsToSeqs(refs)
	}
	return WindowResult{
		Key: ws.key, Status: status, EventSeqs: seqs,
		ObservationCount: len(seqs), Aggregates: aggregates,
		RecomputeCount: ws.recomputeCount,
		Traversal:      rt.mode, PathReasonCode: rt.path.ReasonCode,
	}
}

func (ws *windowRuntime) status(final bool) WindowStatus {
	if final || ws.committed {
		return WindowCommitted
	}
	return WindowOpen
}

func (rt *featureRuntime) rescan(ws *windowRuntime) []eventRef {
	var refs []eventRef
	for _, ref := range rt.events {
		if ref.ObservedAtMs < ws.key.StartMs || ref.ObservedAtMs >= ws.key.EndMs {
			continue
		}
		if ws.accepted[ref.Seq] {
			refs = append(refs, ref)
		}
	}
	sortRefs(refs)
	return refs
}

func (r *runState) persist(opts RunOptions, fingerprint string, final bool) error {
	if opts.Checkpoint == nil {
		return nil
	}
	snapshot := &CheckpointSnapshot{
		EngineVersion: EngineVersion, ProjectFingerprint: fingerprint,
		ProjectID: r.project.ProjectID, SchemaVersion: r.project.SchemaVersion,
		LastEventSeq: r.lastSeq, WatermarkEnabled: r.project.WatermarkEnabled,
	}
	if r.watermark != nil {
		v := *r.watermark
		snapshot.WatermarkMs = &v
	}
	for _, f := range r.project.Features {
		rt := r.features[f.ID]
		if final {
			for _, key := range rt.plan.Windows {
				ws := rt.windows[key.StartMs]
				if ws == nil {
					ws = &windowRuntime{
						key: key, accepted: map[int64]bool{},
						committed: true, acc: newWindowAccumulator(f.Aggregations),
					}
				}
				result := rt.windowResult(ws, true)
				snapshot.Windows = append(snapshot.Windows, PersistedWindow{
					Key: key, Status: WindowCommitted, AcceptedSeqs: result.EventSeqs,
					Aggregates: result.Aggregates, RecomputeCount: result.RecomputeCount,
				})
			}
		} else {
			for _, ws := range rt.windows {
				result := rt.windowResult(ws, false)
				snapshot.Windows = append(snapshot.Windows, PersistedWindow{
					Key: ws.key, Status: result.Status, AcceptedSeqs: result.EventSeqs,
					Aggregates: result.Aggregates, RecomputeCount: result.RecomputeCount,
				})
			}
		}
		snapshot.Discards = append(snapshot.Discards, rt.discards...)
	}
	sortPersistedWindows(snapshot.Windows)
	sortDiscards(snapshot.Discards)
	if err := opts.Checkpoint.Save(snapshot); err != nil {
		return checkpointError("save", err)
	}
	return nil
}

func checkpointError(stage string, err error) error {
	if be, ok := AsBusinessError(err); ok {
		return be
	}
	lower := strings.ToLower(err.Error())
	if strings.Contains(lower, "malformed") || strings.Contains(lower, "not a database") ||
		strings.Contains(lower, "unexpected eof") || strings.Contains(lower, "database disk image") {
		return NewBusinessError(CodeCheckpointTruncated, "checkpoint database is truncated or corrupt").
			WithContext("stage", stage).WithContext("sqlite_error", err.Error()).Wrap(err)
	}
	return NewBusinessError(CodeCheckpointStore, "checkpoint %s failed: %v", stage, err).
		WithContext("stage", stage).Wrap(err)
}

func minInt(a, b int) int {
	if a < b {
		return a
	}
	return b
}
