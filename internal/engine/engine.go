// Package engine coordinates loading, state transitions, bounded parallel
// processing, error injection and atomic snapshot commits.
package engine

import (
	"context"
	"errors"
	"fmt"
	"math"
	"reflect"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"hdrproof/internal/apperr"
	"hdrproof/internal/convert"
	"hdrproof/internal/model"
	"hdrproof/internal/params"
	"hdrproof/internal/sample"
	"hdrproof/internal/snapshot"
	"hdrproof/internal/state"
)

type Mode string

const (
	ModeRealtime Mode = "realtime"
	ModeResume   Mode = "resume"
	ModeFull     Mode = "full"
)

type Config struct {
	DatasetDir               string
	DBPath                   string
	InjectionPath            string
	Mode                     Mode
	Params                   model.Params
	MaxWorkers               int
	FrameBufferPerWorker     int
	CancelAfterCommits       int
	AutoRetryInjectedFailure bool
	ParamSchedule            []ParamUpdate
	EventSink                func(Event)
}

// ParamUpdate applies to uncommitted segments at or after FromSegment. RouteID
// may be "*" for all routes. Committed segments are never rewritten.
type ParamUpdate struct {
	RouteID     string       `json:"route_id"`
	FromSegment int          `json:"from_segment"`
	Params      model.Params `json:"params"`
}

type Event struct {
	TimeUTC string `json:"time_utc"`
	Kind    string `json:"kind"`
	RouteID string `json:"route_id,omitempty"`
	Segment int    `json:"segment,omitempty"`
	Frame   int    `json:"frame,omitempty"`
	Reason  string `json:"reason,omitempty"`
	Message string `json:"message,omitempty"`
}

type RouteStats struct {
	RouteID           string `json:"route_id"`
	Frames            int    `json:"frames"`
	SegmentsCommitted int    `json:"segments_committed"`
	OutputSHA256      string `json:"output_sha256"`
}

type RunResult struct {
	Mode             Mode           `json:"mode"`
	DBPath           string         `json:"db_path"`
	Frames           int            `json:"frames"`
	Records          int            `json:"records"`
	Progress         state.Progress `json:"progress"`
	SnapshotReused   bool           `json:"snapshot_reused"`
	SnapshotReason   string         `json:"snapshot_reason"`
	Cancelled        bool           `json:"cancelled"`
	EventCount       int            `json:"event_count"`
	BufferBlocks     int            `json:"buffer_blocks"`
	Errors           []apperr.Info  `json:"errors,omitempty"`
	RouteStats       []RouteStats   `json:"route_stats"`
	ElapsedMS        int64          `json:"elapsed_ms"`
	MaxWorkers       int            `json:"max_workers"`
	FrameBufferLimit int            `json:"frame_buffer_limit_per_worker"`
	ParamPlanVersion string         `json:"param_plan_version"`
}

type Engine struct {
	cfg     Config
	dataset *model.Dataset
	inject  model.Injections
	store   *snapshot.Store
	plans   []snapshot.RoutePlan
	machine *state.Machine

	mu          sync.Mutex
	events      []Event
	runErrors   []error
	bufferBlock int64
	commits     int64
	cancelled   atomic.Bool
	cancel      context.CancelFunc
	failureOnce map[string]bool
}

func Execute(ctx context.Context, cfg Config) (*RunResult, error) {
	if cfg.Params == (model.Params{}) {
		cfg.Params = model.DefaultParams()
	}
	if cfg.Mode == "" {
		cfg.Mode = ModeRealtime
	}
	if cfg.MaxWorkers <= 0 {
		cfg.MaxWorkers = 4
	}
	if cfg.MaxWorkers > 16 {
		cfg.MaxWorkers = 16
	}
	if cfg.FrameBufferPerWorker <= 0 {
		cfg.FrameBufferPerWorker = 2
	}
	started := time.Now()
	ds, err := sample.Load(cfg.DatasetDir)
	if err != nil {
		return nil, err
	}
	injections, err := sample.LoadInjections(cfg.InjectionPath)
	if err != nil {
		return nil, apperr.New(apperr.CodeMeta, "", -1, -1, "injections",
			"valid injection JSON", err.Error(), err)
	}
	e := &Engine{
		cfg: cfg, dataset: ds, inject: injections,
		failureOnce: make(map[string]bool),
	}
	if err := convert.ValidateParams(cfg.Params, "", -1); err != nil {
		return nil, err
	}
	routes := make([]model.Route, 0, len(ds.Routes))
	segmentsByRoute := make(map[string][]int)
	var routeIDs []string
	for _, rd := range ds.Routes {
		routes = append(routes, rd.Route)
		routeIDs = append(routeIDs, rd.Route.ID)
		count := model.SegmentCount(rd.Route)
		for seg := 0; seg < count; seg++ {
			if len(model.FrameIndicesForSegment(rd.Route, seg)) > 0 {
				segmentsByRoute[rd.Route.ID] = append(segmentsByRoute[rd.Route.ID], seg)
			}
		}
	}
	e.plans = snapshot.BuildPlans(routes)
	e.machine = state.New(routeIDs, segmentsByRoute)
	paramPlanVersion := parameterPlanVersion(cfg.Params, cfg.ParamSchedule)
	meta := snapshot.Meta{
		SchemaVersion: model.SchemaVersion, AlgorithmVersion: model.AlgorithmVersion,
		ParamVersion: paramPlanVersion, TruthFingerprint: ds.TruthFingerprint,
	}
	expectedVersions := make(map[snapshot.SegmentKey]string)
	for _, rp := range e.plans {
		for _, sp := range rp.Segments {
			segmentParams := e.paramsForSegment(rp.Route.ID, sp.Segment)
			if err := convert.ValidateParams(segmentParams, rp.Route.ID, sp.Segment); err != nil {
				return nil, err
			}
			expectedVersions[snapshot.SegmentKey{RouteID: rp.Route.ID, Segment: sp.Segment}] =
				model.ParamVersion(segmentParams)
		}
	}
	store, err := snapshot.Open(cfg.DBPath)
	if err != nil {
		return nil, err
	}
	defer store.Close()
	e.store = store
	loaded, err := store.InitializeWithVersions(ctx, meta, e.plans, cfg.Mode == ModeFull, expectedVersions)
	if err != nil {
		return nil, err
	}
	for key := range loaded.Segments {
		_ = e.machine.TransitionSegment(key.RouteID, key.Segment, state.SegmentRunning)
		_ = e.machine.TransitionSegment(key.RouteID, key.Segment, state.SegmentCommitted)
	}
	e.emit(Event{Kind: "snapshot_loaded", Reason: loaded.Reason,
		Message: fmt.Sprintf("segments=%d frames=%d reused=%v", len(loaded.Segments), len(loaded.Frames), loaded.Reused)})

	runCtx, cancel := context.WithCancel(ctx)
	e.cancel = cancel
	defer cancel()
	rdByID := make(map[string]*model.RouteData, len(ds.Routes))
	for _, rd := range ds.Routes {
		rdByID[rd.Route.ID] = rd
	}
	planByID := make(map[string]snapshot.RoutePlan, len(e.plans))
	for _, rp := range e.plans {
		planByID[rp.Route.ID] = rp
	}
	sem := make(chan struct{}, cfg.MaxWorkers)
	var wg sync.WaitGroup
	for _, routeID := range routeIDs {
		routeID := routeID
		wg.Add(1)
		go func() {
			defer wg.Done()
			select {
			case sem <- struct{}{}:
				defer func() { <-sem }()
			case <-runCtx.Done():
				e.cancelled.Store(true)
				return
			}
			e.processRoute(runCtx, rdByID[routeID], planByID[routeID], loaded)
		}()
	}
	wg.Wait()

	segments, frames, err := store.ReadAll(context.Background())
	if err != nil {
		return nil, err
	}
	result := &RunResult{
		Mode: cfg.Mode, DBPath: cfg.DBPath, Frames: len(frames), Records: len(frames),
		Progress: e.machine.Progress(), SnapshotReused: loaded.Reused,
		SnapshotReason: loaded.Reason, Cancelled: e.cancelled.Load(),
		MaxWorkers: cfg.MaxWorkers, FrameBufferLimit: cfg.FrameBufferPerWorker,
		ParamPlanVersion: paramPlanVersion,
		RouteStats:       buildRouteStats(routes, frames, segments),
	}
	e.mu.Lock()
	result.EventCount = len(e.events)
	result.BufferBlocks = int(atomic.LoadInt64(&e.bufferBlock))
	for _, runErr := range e.runErrors {
		result.Errors = append(result.Errors, apperr.Describe(runErr))
	}
	errs := append([]error(nil), e.runErrors...)
	e.mu.Unlock()
	result.ElapsedMS = time.Since(started).Milliseconds()
	if len(errs) > 0 {
		return result, errors.Join(errs...)
	}
	return result, nil
}

func (e *Engine) emit(ev Event) {
	ev.TimeUTC = time.Now().UTC().Format(time.RFC3339Nano)
	e.mu.Lock()
	e.events = append(e.events, ev)
	sink := e.cfg.EventSink
	e.mu.Unlock()
	if sink != nil {
		sink(ev)
	}
}

func (e *Engine) addError(err error) {
	e.mu.Lock()
	e.runErrors = append(e.runErrors, err)
	e.mu.Unlock()
}

func (e *Engine) processRoute(ctx context.Context, rd *model.RouteData, plan snapshot.RoutePlan, loaded *snapshot.Loaded) {
	routeID := rd.Route.ID
	if err := e.machine.TransitionRoute(routeID, state.RouteRunning); err != nil {
		e.addError(err)
		return
	}
	e.emit(Event{Kind: "route_started", RouteID: routeID})
	for _, segmentPlan := range plan.Segments {
		if ctx.Err() != nil {
			break
		}
		key := snapshot.SegmentKey{RouteID: routeID, Segment: segmentPlan.Segment}
		if _, committed := loaded.Segments[key]; committed {
			continue
		}
		if err := e.machine.TransitionSegment(routeID, segmentPlan.Segment, state.SegmentRunning); err != nil {
			e.addError(err)
			continue
		}
		segmentParams := e.paramsForSegment(routeID, segmentPlan.Segment)
		err := e.processSegmentAttempt(ctx, rd, segmentPlan, segmentParams, false)
		if err != nil {
			if errors.Is(err, context.Canceled) && ctx.Err() != nil {
				e.machine.MarkFailed(routeID, segmentPlan.Segment, err)
				break
			}
			e.failSegment(routeID, segmentPlan.Segment, err)
			if apperr.CodeOf(err) == apperr.CodeSeg && e.cfg.AutoRetryInjectedFailure {
				e.emit(Event{Kind: "retry_started", RouteID: routeID, Segment: segmentPlan.Segment,
					Reason: "controlled_injection_one_shot"})
				if stateErr := e.machine.TransitionSegment(routeID, segmentPlan.Segment, state.SegmentRunning); stateErr == nil {
					if retryErr := e.processSegmentAttempt(ctx, rd, segmentPlan, segmentParams, true); retryErr == nil {
						_ = e.machine.TransitionSegment(routeID, segmentPlan.Segment, state.SegmentCommitted)
						e.emit(Event{Kind: "retry_passed", RouteID: routeID, Segment: segmentPlan.Segment})
					} else {
						e.failSegment(routeID, segmentPlan.Segment, retryErr)
					}
				}
			}
		} else {
			_ = e.machine.TransitionSegment(routeID, segmentPlan.Segment, state.SegmentCommitted)
			e.afterCommit(routeID, segmentPlan.Segment)
		}
	}
	e.finishRoute(ctx, routeID)
}

func (e *Engine) finishRoute(ctx context.Context, routeID string) {
	progress := e.machine.Progress()
	allCommitted := true
	for _, route := range e.machine.View() {
		if route.ID != routeID {
			continue
		}
		for _, seg := range route.Segments {
			if seg.Phase != state.SegmentCommitted {
				allCommitted = false
			}
		}
	}
	phase := e.machine.RoutePhase(routeID)
	if allCommitted {
		if phase == state.RouteFailed || phase == state.RouteCancelled {
			_ = e.machine.TransitionRoute(routeID, state.RouteRunning)
		}
		if e.machine.RoutePhase(routeID) == state.RouteRunning {
			_ = e.machine.TransitionRoute(routeID, state.RouteCompleted)
			e.emit(Event{Kind: "route_completed", RouteID: routeID,
				Message: fmt.Sprintf("segments=%d", progress.SegmentsCommitted)})
		}
		return
	}
	if ctx.Err() != nil {
		if e.machine.RoutePhase(routeID) == state.RouteRunning {
			_ = e.machine.TransitionRoute(routeID, state.RouteCancelled)
		}
		e.cancelled.Store(true)
		return
	}
	if e.machine.RoutePhase(routeID) == state.RouteRunning {
		_ = e.machine.TransitionRoute(routeID, state.RouteFailed)
	}
}

func (e *Engine) failSegment(routeID string, segment int, err error) {
	e.machine.MarkFailed(routeID, segment, err)
	e.addError(err)
	e.emit(Event{Kind: "segment_failed", RouteID: routeID, Segment: segment,
		Reason: string(apperr.CodeOf(err)), Message: err.Error()})
}

func (e *Engine) afterCommit(routeID string, segment int) {
	n := atomic.AddInt64(&e.commits, 1)
	e.emit(Event{Kind: "committed", RouteID: routeID, Segment: segment,
		Message: fmt.Sprintf("commit=%d", n)})
	if e.cfg.CancelAfterCommits > 0 && int(n) >= e.cfg.CancelAfterCommits {
		e.cancelled.Store(true)
		if e.cancel != nil {
			e.cancel()
		}
	}
}

func (e *Engine) processSegmentAttempt(ctx context.Context, rd *model.RouteData, plan snapshot.SegmentPlan, base model.Params, skipFailure bool) error {
	routeID := rd.Route.ID
	segment := plan.Segment
	if conflict := e.findMetaConflict(routeID, segment); conflict != nil {
		actual := routeFieldValue(rd.Route, conflict.Field)
		expected := params.NumberString(conflict.Value)
		return apperr.New(apperr.CodeMeta, routeID, segment, -1, conflict.Field,
			expected, fmt.Sprintf("received %s", actual), nil)
	}
	segmentParams := base
	if injection := e.findParameterInjection(routeID, segment); injection != nil {
		modified, err := params.ApplyInjection(segmentParams, injection.Field, injection.Value)
		if err != nil {
			if ae, ok := apperr.As(err); ok {
				ae.RouteID, ae.Segment = routeID, segment
			}
			return err
		}
		segmentParams = modified
	}
	if err := convert.ValidateParams(segmentParams, routeID, segment); err != nil {
		return err
	}
	if bad := e.findBadFrame(routeID, segment); bad != nil {
		return apperr.New(apperr.CodeFrame, routeID, segment, bad.FrameIndex, bad.Field,
			"valid decoded frame", "controlled bad-frame injection", nil)
	}
	if !skipFailure {
		key := fmt.Sprintf("%s/%d", routeID, segment)
		failure := e.findSegmentFailure(routeID, segment)
		first := false
		if failure != nil {
			e.mu.Lock()
			if !e.failureOnce[key] {
				e.failureOnce[key] = true
				first = true
			}
			e.mu.Unlock()
		}
		if failure != nil && first {
			return apperr.New(apperr.CodeSeg, routeID, segment, -1, failure.Field,
				"successful segment conversion", failure.Message, nil)
		}
	}
	frames := make([]model.DecodedFrame, 0, plan.FrameEnd-plan.FrameStart+1)
	for i := plan.FrameStart; i <= plan.FrameEnd; i++ {
		frames = append(frames, rd.DecodedFrames[i])
	}
	results, err := e.convertWithBuffer(ctx, rd, segment, frames, segmentParams)
	if err != nil {
		return err
	}
	if ctx.Err() != nil {
		return context.Canceled
	}
	_, err = e.store.CommitSegment(ctx, snapshot.Commit{
		Plan: plan, Frames: results, ParamVersion: model.ParamVersion(segmentParams),
		AlgorithmVersion: model.AlgorithmVersion, TruthFingerprint: e.dataset.TruthFingerprint,
	})
	return err
}

func (e *Engine) convertWithBuffer(ctx context.Context, rd *model.RouteData, segment int, frames []model.DecodedFrame, p model.Params) ([]model.FrameResult, error) {
	childCtx, cancel := context.WithCancel(ctx)
	defer cancel()
	ch := make(chan model.DecodedFrame, e.cfg.FrameBufferPerWorker)
	producerDone := make(chan error, 1)
	go func() {
		for _, frame := range frames {
			if len(ch) == cap(ch) {
				atomic.AddInt64(&e.bufferBlock, 1)
				e.emit(Event{Kind: "frame_buffer_blocked", RouteID: rd.Route.ID, Segment: segment,
					Frame: frame.Index, Reason: "frame_buffer_full",
					Message: fmt.Sprintf("worker buffer capacity %d exhausted", cap(ch))})
			}
			select {
			case ch <- frame:
			case <-childCtx.Done():
				producerDone <- childCtx.Err()
				close(ch)
				return
			}
		}
		producerDone <- nil
		close(ch)
	}()
	results := make([]model.FrameResult, 0, len(frames))
	var firstErr error
	for frame := range ch {
		if firstErr != nil {
			continue
		}
		truth := rd.Truth.Frames[frame.Index]
		result, err := convert.ConvertFrame(rd.Route, frame, truth, p, e.dataset.TruthFingerprint)
		if err != nil {
			firstErr = err
			e.emit(Event{Kind: "converted", RouteID: rd.Route.ID, Segment: segment, Frame: frame.Index,
				Reason: "failed", Message: err.Error()})
			cancel()
			continue
		}
		results = append(results, result)
		e.emit(Event{Kind: "converted", RouteID: rd.Route.ID, Segment: segment, Frame: frame.Index})
	}
	prodErr := <-producerDone
	if firstErr != nil {
		return nil, firstErr
	}
	if prodErr != nil && !errors.Is(prodErr, context.Canceled) {
		return nil, prodErr
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if len(results) != len(frames) {
		return nil, apperr.New(apperr.CodeSeg, rd.Route.ID, segment, -1, "frame_buffer",
			fmt.Sprintf("%d converted frames", len(frames)), fmt.Sprintf("received %d", len(results)), nil)
	}
	return results, nil
}

func (e *Engine) findMetaConflict(routeID string, segment int) *model.MetaConflict {
	for i := range e.inject.MetaConflicts {
		c := &e.inject.MetaConflicts[i]
		if c.RouteID == routeID && c.Segment == segment {
			return c
		}
	}
	return nil
}

func (e *Engine) findParameterInjection(routeID string, segment int) *model.ParameterInjection {
	for i := range e.inject.ParameterInjections {
		injection := &e.inject.ParameterInjections[i]
		if injection.RouteID == routeID && injection.Segment == segment {
			return injection
		}
	}
	return nil
}

func (e *Engine) findBadFrame(routeID string, segment int) *model.BadFrame {
	for i := range e.inject.BadFrames {
		bad := &e.inject.BadFrames[i]
		if bad.RouteID == routeID && bad.Segment == segment {
			return bad
		}
	}
	return nil
}

func (e *Engine) findSegmentFailure(routeID string, segment int) *model.SegmentFailure {
	for i := range e.inject.SegmentFailures {
		failure := &e.inject.SegmentFailures[i]
		if failure.RouteID == routeID && failure.Segment == segment {
			return failure
		}
	}
	return nil
}

func (e *Engine) paramsForSegment(routeID string, segment int) model.Params {
	p := e.cfg.Params
	for _, update := range e.cfg.ParamSchedule {
		if segment < update.FromSegment {
			continue
		}
		if update.RouteID == "" || update.RouteID == "*" || update.RouteID == routeID {
			p = update.Params
		}
	}
	return p
}

func parameterPlanVersion(base model.Params, updates []ParamUpdate) string {
	if len(updates) == 0 {
		return model.ParamVersion(base)
	}
	return model.HashJSON(struct {
		Base    model.Params  `json:"base"`
		Updates []ParamUpdate `json:"updates"`
	}{Base: base, Updates: updates})
}

func routeFieldValue(route model.Route, field string) string {
	v := reflect.ValueOf(route)
	t := v.Type()
	target := strings.ToLower(field)
	for i := 0; i < t.NumField(); i++ {
		sf := t.Field(i)
		jsonName := strings.Split(sf.Tag.Get("json"), ",")[0]
		if strings.EqualFold(jsonName, field) || strings.EqualFold(sf.Name, field) || strings.ToLower(sf.Name) == target {
			f := v.Field(i)
			switch f.Kind() {
			case reflect.Int, reflect.Int8, reflect.Int16, reflect.Int32, reflect.Int64:
				return fmt.Sprint(f.Int())
			case reflect.Float32, reflect.Float64:
				n := f.Float()
				if math.Trunc(n) == n {
					return fmt.Sprintf("%.0f", n)
				}
				return fmt.Sprint(n)
			case reflect.String:
				return f.String()
			}
		}
	}
	return "<missing>"
}

func buildRouteStats(routes []model.Route, frames map[snapshot.FrameKey]snapshot.StoredFrame, segments map[snapshot.SegmentKey]snapshot.StoredSegment) []RouteStats {
	out := make([]RouteStats, 0, len(routes))
	for _, route := range routes {
		stat := RouteStats{RouteID: route.ID}
		var ordered []snapshot.StoredFrame
		for key, frame := range frames {
			if key.RouteID == route.ID {
				ordered = append(ordered, frame)
			}
		}
		sort.Slice(ordered, func(i, j int) bool { return ordered[i].FrameIndex < ordered[j].FrameIndex })
		h := newHasher()
		for _, frame := range ordered {
			h.add(frame.PixelHash)
		}
		stat.Frames = len(ordered)
		stat.OutputSHA256 = h.sum()
		for key := range segments {
			if key.RouteID == route.ID {
				stat.SegmentsCommitted++
			}
		}
		out = append(out, stat)
	}
	return out
}

type hasher struct{ parts []string }

func newHasher() *hasher       { return &hasher{} }
func (h *hasher) add(s string) { h.parts = append(h.parts, s) }
func (h *hasher) sum() string  { return model.SHA256Hex([]byte(strings.Join(h.parts, ""))) }
