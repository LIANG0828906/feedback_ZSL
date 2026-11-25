package vpcompose

import (
	"context"
	"fmt"
	"sort"
	"sync"
	"time"
)

type RunMode string

const (
	ModeRealtime RunMode = "realtime"
	ModeRecover  RunMode = "recover"
	ModeFull     RunMode = "full"
)

type EngineOptions struct {
	Workers            int
	MaxPixelsPerWorker int
	FaultInjector      func(window int) error
}

type RecoveryReport struct {
	Committed   []int `json:"committed"`
	Invalidated bool  `json:"invalidated"`
}

type RunReport struct {
	Mode        RunMode        `json:"mode"`
	Progress    WindowProgress `json:"progress"`
	Committed   []int          `json:"committed"`
	Failed      []int          `json:"failed"`
	Errors      map[int]string `json:"errors,omitempty"`
	QueueEvents []QueueEvent   `json:"queueEvents,omitempty"`
	Recovery    RecoveryReport `json:"recovery"`
}

type Engine struct {
	mu          sync.Mutex
	project     *Project
	windows     []*Window
	store       *CheckpointStore
	options     EngineOptions
	deps        *DependencyGraph
	queueEvents []QueueEvent
	running     bool
}

func NewEngine(project *Project, store *CheckpointStore, options EngineOptions) (*Engine, error) {
	if project == nil {
		return nil, schemaError("project", "nil project")
	}
	clone := project.Clone()
	if err := clone.Normalize(); err != nil {
		return nil, err
	}
	if err := clone.ValidateHierarchy(); err != nil {
		return nil, err
	}
	if options.Workers <= 0 {
		options.Workers = 2
	}
	if options.MaxPixelsPerWorker <= 0 {
		options.MaxPixelsPerWorker = 1 << 20
	}
	engine := &Engine{
		project: clone, store: store, options: options,
		deps: BuildDependencies(clone),
	}
	for i := 0; i < clone.WindowCount(); i++ {
		engine.windows = append(engine.windows, NewWindow(i, clone))
	}
	return engine, nil
}

func (e *Engine) Project() *Project {
	e.mu.Lock()
	defer e.mu.Unlock()
	return e.project.Clone()
}

func (e *Engine) Windows() []*Window {
	e.mu.Lock()
	defer e.mu.Unlock()
	out := make([]*Window, len(e.windows))
	copy(out, e.windows)
	return out
}

func (e *Engine) Dependencies() *DependencyGraph {
	e.mu.Lock()
	defer e.mu.Unlock()
	return e.deps
}

func (e *Engine) Progress() WindowProgress {
	e.mu.Lock()
	defer e.mu.Unlock()
	return Progress(e.windows)
}

func (e *Engine) Recover() (RecoveryReport, error) {
	e.mu.Lock()
	defer e.mu.Unlock()
	return e.recoverLocked()
}

func (e *Engine) recoverLocked() (RecoveryReport, error) {
	report := RecoveryReport{}
	if e.store == nil {
		return report, nil
	}
	records, invalidated, err := e.store.LoadCommitted(e.project.OverallVersion())
	report.Invalidated = invalidated
	if err != nil {
		return report, err
	}
	if invalidated {
		for _, window := range e.windows {
			window.State = WindowPending
			window.Snapshot = nil
			window.Result = nil
			window.LastError = ""
		}
		return report, nil
	}
	for _, record := range records {
		if record.WindowIndex < 0 || record.WindowIndex >= len(e.windows) {
			if err := e.store.Reset(); err != nil {
				return report, err
			}
			report.Invalidated = true
			return report, nil
		}
		window := e.windows[record.WindowIndex]
		if record.FrameStart != window.FrameStart || record.FrameEnd != window.FrameEnd ||
			record.AlgorithmVersion != e.project.AlgorithmVersion ||
			record.SourceVersion != e.project.SourceVersion ||
			record.ContentHash != record.Snapshot.ContentHash ||
			record.ParamVersion != record.Snapshot.ParamVersion ||
			record.Snapshot.WindowIndex != record.WindowIndex {
			if err := e.store.Reset(); err != nil {
				return report, err
			}
			report.Invalidated = true
			for _, w := range e.windows {
				w.State = WindowPending
				w.Snapshot, w.Result = nil, nil
			}
			return report, nil
		}
		window.State = WindowCommitted
		window.Snapshot = record.Snapshot
		window.Result = record.Result
		window.CommitOrder = record.WindowIndex
		report.Committed = append(report.Committed, record.WindowIndex)
	}
	sort.Ints(report.Committed)
	return report, nil
}

func (e *Engine) Run(ctx context.Context, mode RunMode) (*RunReport, error) {
	e.mu.Lock()
	defer e.mu.Unlock()
	return e.runLocked(ctx, mode)
}

func (e *Engine) runLocked(ctx context.Context, mode RunMode) (*RunReport, error) {
	if e.running {
		return nil, &EngineError{Code: CodeState, Path: "engine.running", Message: "engine is already running"}
	}
	report := &RunReport{Mode: mode, Errors: make(map[int]string)}
	switch mode {
	case ModeRecover:
		recovery, err := e.recoverLocked()
		if err != nil {
			return nil, err
		}
		report.Recovery = recovery
	case ModeFull:
		if e.store != nil {
			if err := e.store.Reset(); err != nil {
				return nil, err
			}
		}
		for _, window := range e.windows {
			window.State = WindowPending
			window.Snapshot = nil
			window.Result = nil
			window.LastError = ""
			window.CommitOrder = 0
		}
	case ModeRealtime:
	default:
		return nil, schemaError("mode", "unknown run mode")
	}
	var jobs []*Window
	for _, window := range e.windows {
		if window.State == WindowPending {
			jobs = append(jobs, window)
		}
	}
	e.running = true
	queueEvents, err := e.runWindows(ctx, jobs, report)
	e.running = false
	e.queueEvents = queueEvents
	report.QueueEvents = append([]QueueEvent(nil), queueEvents...)
	report.Progress = Progress(e.windows)
	report.Committed = committedIndexes(e.windows)
	report.Failed = failedIndexes(e.windows)
	if err != nil {
		return report, err
	}
	return report, nil
}

func committedIndexes(windows []*Window) []int {
	var out []int
	for _, window := range windows {
		if window.State == WindowCommitted {
			out = append(out, window.Index)
		}
	}
	sort.Ints(out)
	return out
}

func failedIndexes(windows []*Window) []int {
	var out []int
	for _, window := range windows {
		if window.State == WindowFailed {
			out = append(out, window.Index)
		}
	}
	sort.Ints(out)
	return out
}

type composeJob struct {
	window *Window
}

type composeOutcome struct {
	window   int
	worker   int
	snapshot *StackSnapshot
	result   *WindowResult
	err      error
}

func (e *Engine) runWindows(ctx context.Context, jobs []*Window, report *RunReport) ([]QueueEvent, error) {
	if len(jobs) == 0 {
		return nil, nil
	}
	workerCount := e.options.Workers
	if workerCount > len(jobs) {
		workerCount = len(jobs)
	}
	jobCh := make(chan composeJob, len(jobs))
	outCh := make(chan composeOutcome, len(jobs))
	slots := make(chan struct{}, workerCount)
	var wg sync.WaitGroup
	var events []QueueEvent
	for workerID := 0; workerID < workerCount; workerID++ {
		wg.Add(1)
		go func(id int) {
			defer wg.Done()
			for job := range jobCh {
				outcome := e.composeOne(ctx, job.window, id)
				<-slots
				outCh <- outcome
			}
		}(workerID)
	}
	go func() {
		wg.Wait()
		close(outCh)
	}()
	canceled := false
	for _, window := range jobs {
		started := time.Now()
		blocked := false
		select {
		case slots <- struct{}{}:
		default:
			blocked = true
			select {
			case slots <- struct{}{}:
			case <-ctx.Done():
				canceled = true
			}
		}
		if canceled {
			break
		}
		if blocked {
			events = append(events, QueueEvent{
				Window: window.Index, Reason: "worker-or-pixel-buffer-capacity",
				Wait: time.Since(started), Worker: -1,
			})
		}
		jobCh <- composeJob{window: window}
	}
	close(jobCh)

	var outcomes []composeOutcome
	for outcome := range outCh {
		outcomes = append(outcomes, outcome)
	}
	if err := ctx.Err(); err != nil {
		for _, window := range jobs {
			if window.State == WindowCompositing {
				_ = window.Fail(err)
			}
		}
		return events, err
	}
	if canceled {
		return events, context.Canceled
	}
	sort.Slice(outcomes, func(i, j int) bool { return outcomes[i].window < outcomes[j].window })
	for _, outcome := range outcomes {
		if outcome.err != nil {
			report.Errors[outcome.window] = outcome.err.Error()
			continue
		}
		window := e.windows[outcome.window]
		if window.State != WindowCompositing {
			report.Errors[outcome.window] = "window left compositing state unexpectedly"
			continue
		}
		window.Snapshot = outcome.snapshot
		record := &CheckpointRecord{
			WindowIndex: window.Index, FrameStart: window.FrameStart, FrameEnd: window.FrameEnd,
			State: WindowCommitted, Snapshot: outcome.snapshot, Result: outcome.result,
			Digest: outcome.result.Digest, ContentHash: e.project.ContentHash(),
			SourceVersion: e.project.SourceVersion, ParamVersion: window.Snapshot.ParamVersion,
			AlgorithmVersion: e.project.AlgorithmVersion,
			OverallVersion:   e.project.OverallVersion(),
		}
		if e.store != nil {
			if err := e.store.SaveCommitted(record); err != nil {
				if failErr := window.Fail(err); failErr != nil {
					return events, failErr
				}
				report.Errors[window.Index] = err.Error()
				continue
			}
		}
		if err := window.Commit(outcome.result); err != nil {
			return events, err
		}
	}
	return events, nil
}

func (e *Engine) composeOne(ctx context.Context, window *Window, worker int) (outcome composeOutcome) {
	outcome.window = window.Index
	outcome.worker = worker
	defer func() {
		if recovered := recover(); recovered != nil {
			err := &EngineError{
				Code: CodeState, Window: window.Index,
				Message: fmt.Sprintf("compositor panic isolated: %v", recovered),
			}
			if window.State == WindowCompositing {
				_ = window.Fail(err)
			}
			outcome.err = err
			outcome.result = nil
		}
	}()
	select {
	case <-ctx.Done():
		outcome.err = ctx.Err()
		if window.State == WindowPending || window.State == WindowFailed {
			_ = window.Start()
			_ = window.Fail(outcome.err)
		}
		return outcome
	default:
	}
	if err := window.Start(); err != nil {
		outcome.err = err
		return outcome
	}
	snapshot, err := CaptureWindowSnapshot(e.project, window.Index)
	if err != nil {
		_ = window.Fail(err)
		outcome.err = err
		return outcome
	}
	window.Snapshot = snapshot
	outcome.snapshot = snapshot
	requiredPixels := e.project.Width * e.project.Height * e.project.Channels
	if requiredPixels > e.options.MaxPixelsPerWorker {
		err := &EngineError{
			Code: CodeBudget, Path: "worker.maxPixels",
			Message:  "per-worker pixel buffer limit is smaller than one frame",
			Operands: []int64{int64(requiredPixels), int64(e.options.MaxPixelsPerWorker)},
			Window:   window.Index,
		}
		_ = window.Fail(err)
		outcome.err = err
		return outcome
	}
	if e.options.FaultInjector != nil {
		if err := e.options.FaultInjector(window.Index); err != nil {
			_ = window.Fail(err)
			outcome.err = err
			return outcome
		}
	}
	result, err := BuildWindowResult(e.project, snapshot, window.Index)
	if err != nil {
		_ = window.Fail(err)
		outcome.err = err
		return outcome
	}
	outcome.result = result
	return outcome
}

func (e *Engine) RetryFailed(ctx context.Context) (*RunReport, error) {
	e.mu.Lock()
	defer e.mu.Unlock()
	report := &RunReport{Mode: "retry", Errors: make(map[int]string)}
	var jobs []*Window
	for _, window := range e.windows {
		if window.State == WindowFailed {
			jobs = append(jobs, window)
		}
	}
	events, err := e.runWindows(ctx, jobs, report)
	report.QueueEvents = events
	report.Progress = Progress(e.windows)
	report.Committed = committedIndexes(e.windows)
	report.Failed = failedIndexes(e.windows)
	return report, err
}

func (e *Engine) ComposeWindow(ctx context.Context, index int) (*RunReport, error) {
	e.mu.Lock()
	defer e.mu.Unlock()
	if index < 0 || index >= len(e.windows) {
		return nil, schemaError("windowIndex", "outside project windows")
	}
	window := e.windows[index]
	if window.State == WindowCommitted {
		return &RunReport{
			Mode: "window", Progress: Progress(e.windows),
			Committed: committedIndexes(e.windows), Failed: failedIndexes(e.windows),
			Errors: make(map[int]string), QueueEvents: []QueueEvent{},
		}, nil
	}
	report := &RunReport{Mode: "window", Errors: make(map[int]string)}
	events, err := e.runWindows(ctx, []*Window{window}, report)
	report.QueueEvents = events
	report.Progress = Progress(e.windows)
	report.Committed = committedIndexes(e.windows)
	report.Failed = failedIndexes(e.windows)
	return report, err
}

func (e *Engine) EditLayer(edit LayerEdit) (*InvalidationReport, error) {
	e.mu.Lock()
	defer e.mu.Unlock()
	if e.running {
		return nil, &EngineError{Code: CodeState, Path: "engine.running", Message: "cannot edit while composing"}
	}
	layer := e.project.LayerByID(edit.LayerID)
	if layer == nil {
		return nil, schemaError("edit.layerId", "unknown layer "+edit.LayerID)
	}
	oldLayers := cloneLayers(e.project.Layers)
	changed := false
	if edit.Opacity != nil && *edit.Opacity != layer.Opacity {
		layer.Opacity = *edit.Opacity
		changed = true
	}
	if edit.Visible != nil && *edit.Visible != layer.Visible {
		layer.Visible = *edit.Visible
		changed = true
	}
	if edit.VisibleUntil != nil && *edit.VisibleUntil != layer.VisibleUntil {
		if *edit.VisibleUntil < 0 {
			return nil, schemaError("edit.visibleUntilTick", "must not be negative")
		}
		layer.VisibleUntil = *edit.VisibleUntil
		changed = true
	}
	if edit.BlendMode != nil && *edit.BlendMode != layer.BlendMode {
		if !edit.BlendMode.Valid() {
			return nil, schemaError("edit.blendMode", "unsupported blend mode")
		}
		layer.BlendMode = *edit.BlendMode
		changed = true
	}
	if edit.ContentHash != nil && *edit.ContentHash != layer.ContentHash {
		if *edit.ContentHash == "" {
			return nil, schemaError("edit.contentHash", "must not be empty")
		}
		layer.ContentHash = *edit.ContentHash
		changed = true
	}
	if !changed {
		return &InvalidationReport{}, nil
	}
	layer.ParamVersion++
	e.project.ParamVersion++
	e.deps = BuildDependencies(e.project)
	report := e.invalidateForLayerEdit(edit.LayerID, oldLayers, layer)
	return report, nil
}

func (e *Engine) EditTimebase(timebase Timebase) (*InvalidationReport, error) {
	e.mu.Lock()
	defer e.mu.Unlock()
	if e.running {
		return nil, &EngineError{Code: CodeState, Path: "engine.running", Message: "cannot edit while composing"}
	}
	if timebase.TickRate <= 0 || !timebase.Rounding.Valid() {
		return nil, schemaError("edit.timebase", "invalid tick rate or rounding")
	}
	if timebase == e.project.Timebase {
		return &InvalidationReport{}, nil
	}
	e.project.Timebase = timebase
	e.project.ParamVersion++
	e.deps = BuildDependencies(e.project)
	report := &InvalidationReport{}
	for _, window := range e.windows {
		if window.State == WindowCommitted {
			continue
		}
		window.Result = nil
		report.Entries = append(report.Entries, InvalidationEntry{
			Window: window.Index, LayerIDs: e.project.LayerOrder(), Reason: "timebase",
			Downstream: []string{"window-cache", "checkpoint", "export"},
		})
	}
	return report, nil
}

func (e *Engine) invalidateForLayerEdit(layerID string, oldLayers []*Layer, newLayer *Layer) *InvalidationReport {
	report := &InvalidationReport{}
	for _, window := range e.windows {
		if window.State == WindowCommitted {
			continue
		}
		affected := false
		reason := ""
		for frameIndex := window.FrameStart; frameIndex < window.FrameEnd; frameIndex++ {
			spec := e.project.Frames[frameIndex]
			oldStates, _ := layersAtFrame(oldLayers, e.project.Instructions, spec.TimeTicks)
			newStates, _ := layersAtFrame(e.project.Layers, e.project.Instructions, spec.TimeTicks)
			oldLayer := findLayerState(oldStates, layerID)
			newLayerState := findLayerState(newStates, layerID)
			if oldLayer == nil || newLayerState == nil {
				continue
			}
			oldVisible := layerVisible(*oldLayer, spec.TimeTicks)
			newVisible := layerVisible(*newLayerState, spec.TimeTicks)
			if oldVisible != newVisible {
				affected, reason = true, "visibility"
				break
			}
			if oldVisible || newVisible {
				if oldLayer.Opacity != newLayerState.Opacity {
					affected, reason = true, "opacity"
					break
				}
				if oldLayer.BlendMode != newLayerState.BlendMode {
					affected, reason = true, "blendMode"
					break
				}
				if oldLayer.ContentHash != newLayerState.ContentHash {
					affected, reason = true, "content"
					break
				}
			}
		}
		if affected {
			window.Result = nil
			report.Entries = append(report.Entries, InvalidationEntry{
				Window: window.Index, LayerIDs: []string{layerID}, Reason: reason,
				Downstream: []string{"window-cache", "checkpoint", "export"},
			})
		}
	}
	return report
}

type LayerEdit struct {
	LayerID      string
	Opacity      *uint16
	Visible      *bool
	VisibleUntil *int64
	BlendMode    *BlendMode
	ContentHash  *string
}

func cloneLayers(layers []*Layer) []*Layer {
	out := make([]*Layer, 0, len(layers))
	for _, layer := range layers {
		copyLayer := *layer
		out = append(out, &copyLayer)
	}
	return out
}

func findLayerState(states []layerFrameState, id string) *Layer {
	for i := range states {
		if states[i].layer.ID == id {
			return &states[i].layer
		}
	}
	return nil
}

func (e *Engine) ResultForWindow(index int) (*WindowResult, bool) {
	e.mu.Lock()
	defer e.mu.Unlock()
	if index < 0 || index >= len(e.windows) || e.windows[index].State != WindowCommitted {
		return nil, false
	}
	result := e.windows[index].Result
	return result, result != nil
}

func (e *Engine) Results() map[int]*WindowResult {
	e.mu.Lock()
	defer e.mu.Unlock()
	out := make(map[int]*WindowResult)
	for _, window := range e.windows {
		if window.State == WindowCommitted && window.Result != nil {
			out[window.Index] = window.Result
		}
	}
	return out
}

func (e *Engine) WindowState(index int) (WindowState, error) {
	e.mu.Lock()
	defer e.mu.Unlock()
	if index < 0 || index >= len(e.windows) {
		return "", schemaError("windowIndex", "outside project windows")
	}
	return e.windows[index].State, nil
}

func (e *Engine) CheckpointCount() (int, error) {
	if e.store == nil {
		return 0, nil
	}
	records, _, err := e.store.LoadCommitted(e.project.OverallVersion())
	if err != nil {
		return 0, err
	}
	return len(records), nil
}
