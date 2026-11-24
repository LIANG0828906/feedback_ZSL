package pipeline

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"sync"
	"time"
)

type EngineConfig struct {
	WindowSize     int
	MaxWorkers     int
	MaxFrameBytes  int64
	CheckpointPath string
	FrameDir       string
	StateDir       string
}

func (c EngineConfig) Validate() error {
	if c.WindowSize <= 0 {
		return paraError(-1, "window size must be positive")
	}
	if c.MaxWorkers <= 0 || c.MaxWorkers > 64 {
		return paraError(-1, "max workers must be in [1,64]")
	}
	if c.MaxFrameBytes <= 0 {
		return paraError(-1, "max frame bytes must be positive")
	}
	if c.CheckpointPath == "" || c.FrameDir == "" {
		return paraError(-1, "checkpoint path and frame directory are required")
	}
	return nil
}

type Engine struct {
	runMu sync.Mutex

	modelMu    sync.RWMutex
	model      *Model
	sourceHash string
	modelHash  string

	windows *WindowSet
	store   *CheckpointStore
	frames  FrameStore
	cfg     EngineConfig

	lastInvalidation InvalidationList
	lastRecovery     RecoveryResult
}

func OpenEngine(model *Model, cfg EngineConfig) (*Engine, error) {
	if err := cfg.Validate(); err != nil {
		return nil, err
	}
	if model == nil {
		return nil, paraError(-1, "model is nil")
	}
	model = model.Clone()
	model.Normalize()
	if err := model.Validate(); err != nil {
		return nil, err
	}
	windows, err := NewWindowSet(model.FrameCount, cfg.WindowSize)
	if err != nil {
		return nil, err
	}
	store, err := OpenCheckpoint(cfg.CheckpointPath)
	if err != nil {
		return nil, err
	}
	sourceHash, err := SourceFingerprint(model)
	if err != nil {
		store.Close()
		return nil, err
	}
	modelHash, err := ModelHash(model)
	if err != nil {
		store.Close()
		return nil, err
	}
	e := &Engine{
		model: model, sourceHash: sourceHash, modelHash: modelHash,
		windows: windows, store: store, frames: FrameStore{Dir: cfg.FrameDir}, cfg: cfg,
	}
	recovery, err := store.LoadVerified(RecoveryExpectation{
		AlgorithmVersion: AlgorithmVersion,
		SourceHash:       sourceHash,
		Windows:          windows.Windows(),
		FrameStore:       e.frames,
	})
	if err != nil {
		store.Close()
		return nil, err
	}
	e.lastRecovery = recovery
	if recovery.Purged {
		_ = e.frames.Clear()
	}
	for _, snapshot := range recovery.Restored {
		if _, err := e.frames.ReadWindow(snapshot.WindowIndex); err != nil {
			// A packet that cannot be decoded is treated exactly like a bad
			// hash: no partial old cache may be mixed into the current run.
			if err := store.Clear(); err != nil {
				store.Close()
				return nil, err
			}
			recovery.Purged = true
			recovery.Reason = fmt.Sprintf("window %d packet decode failed: %v", snapshot.WindowIndex, err)
			recovery.Restored = nil
			e.lastRecovery = recovery
			_ = e.frames.Clear()
			break
		}
	}
	if !recovery.Purged {
		for _, snapshot := range recovery.Restored {
			if err := windows.Restore(snapshot.WindowIndex, snapshot); err != nil {
				store.Close()
				return nil, err
			}
		}
	}
	if pending, err := e.readPendingInvalidation(); err != nil {
		store.Close()
		return nil, err
	} else if len(pending.Affected) > 0 {
		for _, inv := range pending.Affected {
			if err := windows.Invalidate(inv.Window, inv); err != nil {
				store.Close()
				return nil, err
			}
		}
		e.lastInvalidation = pending
	}
	return e, nil
}

func (e *Engine) Close() error {
	e.runMu.Lock()
	defer e.runMu.Unlock()
	return e.store.Close()
}

func (e *Engine) Model() *Model {
	e.modelMu.RLock()
	defer e.modelMu.RUnlock()
	return e.model.Clone()
}

func (e *Engine) SourceHash() string {
	e.modelMu.RLock()
	defer e.modelMu.RUnlock()
	return e.sourceHash
}

func (e *Engine) ModelHash() string {
	e.modelMu.RLock()
	defer e.modelMu.RUnlock()
	return e.modelHash
}

func (e *Engine) Windows() []Window {
	return e.windows.Windows()
}

func (e *Engine) Progress() (int, int) {
	return e.windows.Progress()
}

func (e *Engine) Recovery() RecoveryResult {
	return e.lastRecovery
}

func (e *Engine) LastInvalidation() InvalidationList {
	return e.lastInvalidation
}

func (e *Engine) RunFull(ctx context.Context) error {
	e.runMu.Lock()
	defer e.runMu.Unlock()
	model := e.Model()
	if err := model.Validate(); err != nil {
		return err
	}
	e.clearPendingInvalidation()
	e.lastRecovery = RecoveryResult{}
	if err := e.store.Clear(); err != nil {
		return err
	}
	if err := e.frames.Clear(); err != nil {
		return err
	}
	for i := 0; i < e.windows.Len(); i++ {
		if err := e.windows.Reset(i); err != nil {
			return err
		}
	}
	e.lastInvalidation = InvalidationList{Operation: "full", TotalWindows: e.windows.Len(), TotalFrames: model.FrameCount}
	indices := make([]int, e.windows.Len())
	for i := range indices {
		indices[i] = i
	}
	return e.processWindows(ctx, model, indices, false)
}

type EditKind string

const (
	EditLayerParameter  EditKind = "layer_parameter"
	EditLayerVisibility EditKind = "layer_visibility"
	EditTimeBase        EditKind = "time_base"
)

type EditRequest struct {
	Kind     EditKind    `json:"kind"`
	LayerID  string      `json:"layer_id,omitempty"`
	Opacity  *float64    `json:"opacity,omitempty"`
	Alpha    *float64    `json:"alpha,omitempty"`
	Color    *[3]float64 `json:"color,omitempty"`
	Visible  *bool       `json:"visible,omitempty"`
	TimeBase *TimeBase   `json:"time_base,omitempty"`
}

// ApplyIncremental validates and applies one edit, invalidates only the windows
// intersecting the layer's old/new lifecycle, then recomputes those windows.
func (e *Engine) ApplyIncremental(ctx context.Context, edit EditRequest) (InvalidationList, error) {
	e.runMu.Lock()
	defer e.runMu.Unlock()
	oldModel := e.Model()
	newModel := oldModel.Clone()
	affectedFrames, layerIDs, reason, err := applyEdit(newModel, oldModel, edit)
	if err != nil {
		return InvalidationList{}, err
	}
	newModel.Normalize()
	if err := newModel.Validate(); err != nil {
		return InvalidationList{}, err
	}
	newSourceHash, err := SourceFingerprint(newModel)
	if err != nil {
		return InvalidationList{}, err
	}
	newModelHash, err := ModelHash(newModel)
	if err != nil {
		return InvalidationList{}, err
	}
	affectedWindows := e.windowsForFrames(affectedFrames)
	list := InvalidationList{
		Operation: string(edit.Kind), TotalWindows: e.windows.Len(), TotalFrames: newModel.FrameCount,
		Affected: make([]Invalidation, 0, len(affectedWindows)),
		Reused:   []int{},
	}
	for _, index := range affectedWindows {
		list.Affected = append(list.Affected, Invalidation{
			Window: index, LayerIDs: append([]string(nil), layerIDs...), Reason: reason, Stage: "composite+export",
		})
	}
	for i := 0; i < e.windows.Len(); i++ {
		if !containsInt(affectedWindows, i) {
			window, _ := e.windows.Get(i)
			if window.State == WindowCommitted {
				list.Reused = append(list.Reused, i)
			}
		}
	}
	if newSourceHash != e.sourceHash {
		// Source time-base/version changes are global. Purge all checkpoints
		// before the first new commit so old metadata can never be mixed in.
		if err := e.store.Clear(); err != nil {
			return InvalidationList{}, err
		}
		if err := e.frames.Clear(); err != nil {
			return InvalidationList{}, err
		}
		affectedWindows = allWindowIndices(e.windows.Len())
		list.Affected = list.Affected[:0]
		list.Reused = list.Reused[:0]
		for _, index := range affectedWindows {
			list.Affected = append(list.Affected, Invalidation{
				Window: index, LayerIDs: append([]string(nil), layerIDs...), Reason: reason, Stage: "composite+export",
			})
		}
	}
	// Persist the invalidation intent before the edited model. A crash in
	// between leaves an old model with an explicit recompute request, never a
	// new model that silently trusts old snapshots.
	if err := e.persistPendingInvalidation(list); err != nil {
		return InvalidationList{}, err
	}
	if err := e.persistState(newModel); err != nil {
		return InvalidationList{}, err
	}
	if newSourceHash == e.sourceHash && len(affectedWindows) > 0 {
		if err := e.store.DeleteSnapshots(affectedWindows); err != nil {
			return InvalidationList{}, err
		}
	}
	e.setModel(newModel, newSourceHash, newModelHash)
	for _, index := range affectedWindows {
		if err := e.windows.Invalidate(index, Invalidation{
			Window: index, LayerIDs: append([]string(nil), layerIDs...), Reason: reason, Stage: "composite+export",
		}); err != nil {
			return InvalidationList{}, err
		}
	}
	for _, index := range list.Reused {
		_ = e.windows.MarkReused(index)
	}
	e.lastInvalidation = list
	if err := e.processWindows(ctx, newModel, affectedWindows, true); err != nil {
		return list, err
	}
	e.clearPendingInvalidation()
	return list, nil
}

func applyEdit(next, old *Model, edit EditRequest) (map[int]struct{}, []string, string, error) {
	switch edit.Kind {
	case EditLayerParameter, EditLayerVisibility:
		if edit.LayerID == "" {
			return nil, nil, "", layerError("", "edit is missing a layer id")
		}
		oldLayer, ok := old.Layer(edit.LayerID)
		if !ok {
			return nil, nil, "", layerError(edit.LayerID, "edit references a missing layer")
		}
		newLayer, _ := next.Layer(edit.LayerID)
		if edit.Kind == EditLayerVisibility {
			if edit.Visible == nil {
				return nil, nil, "", paraError(-1, "visibility edit requires visible")
			}
			newLayer.Visible = *edit.Visible
		} else {
			if edit.Opacity == nil && edit.Alpha == nil && edit.Color == nil {
				return nil, nil, "", paraError(-1, "parameter edit requires opacity, alpha or color")
			}
			if edit.Opacity != nil {
				if *edit.Opacity < 0 || *edit.Opacity > 1 {
					return nil, nil, "", paraError(-1, "opacity must be in [0,1]")
				}
				newLayer.Opacity = *edit.Opacity
			}
			if edit.Alpha != nil {
				if *edit.Alpha < 0 || *edit.Alpha > 1 {
					return nil, nil, "", paraError(-1, "alpha must be in [0,1]")
				}
				newLayer.Alpha = *edit.Alpha
			}
			if edit.Color != nil {
				for i, value := range *edit.Color {
					if value < 0 || value > 1 {
						return nil, nil, "", paraError(-1, "color[%d] must be in [0,1]", i)
					}
				}
				newLayer.Color = *edit.Color
			}
		}
		affected := old.layerAffectedFrames(*oldLayer)
		for frame := range next.layerAffectedFrames(*newLayer) {
			affected[frame] = struct{}{}
		}
		reason := "layer parameter changed"
		if edit.Kind == EditLayerVisibility {
			reason = "layer visibility changed"
		}
		return affected, []string{edit.LayerID}, reason, nil
	case EditTimeBase:
		if edit.TimeBase == nil {
			return nil, nil, "", paraError(-1, "time base edit requires time_base")
		}
		tb := *edit.TimeBase
		if tb.Unit == "" {
			tb.Unit = next.TimeBase.Unit
		}
		switch tb.Unit {
		case "second", "seconds":
			tb.Unit = "second"
		case "tick", "ticks":
			tb.Unit = "tick"
		default:
			return nil, nil, "", paraError(-1, "unsupported time base unit %q", tb.Unit)
		}
		if tb.FramePeriod.Num <= 0 || tb.FramePeriod.Den <= 0 {
			return nil, nil, "", paraError(-1, "time base period must be positive")
		}
		if tb.Unit == "tick" && (tb.TickRate <= 0 || tb.TicksPerFrame <= 0) {
			return nil, nil, "", paraError(-1, "tick time base requires tick_rate and ticks_per_frame")
		}
		next.TimeBase = tb
		affected := map[int]struct{}{}
		for frame := 0; frame < next.FrameCount; frame++ {
			affected[frame] = struct{}{}
		}
		ids := make([]string, 0, len(next.Layers))
		for _, layer := range next.Layers {
			ids = append(ids, layer.ID)
		}
		return affected, ids, "time base changed", nil
	default:
		return nil, nil, "", paraError(-1, "unsupported edit kind %q", edit.Kind)
	}
}

func (e *Engine) windowsForFrames(frames map[int]struct{}) []int {
	set := map[int]bool{}
	for frame := range frames {
		if index, err := e.windows.WindowForFrame(frame); err == nil {
			set[index] = true
		}
	}
	out := make([]int, 0, len(set))
	for index := range set {
		out = append(out, index)
	}
	sort.Ints(out)
	return out
}

func (e *Engine) processWindows(ctx context.Context, model *Model, indices []int, incremental bool) error {
	sort.Ints(indices)
	batchSize := e.cfg.MaxWorkers
	for start := 0; start < len(indices); start += batchSize {
		end := start + batchSize
		if end > len(indices) {
			end = len(indices)
		}
		batch := append([]int(nil), indices[start:end]...)
		type result struct {
			window Window
			data   *WindowResult
			err    error
		}
		results := make([]result, 0, len(batch))
		var mu sync.Mutex
		var wg sync.WaitGroup
		for _, index := range batch {
			window, err := e.windows.Begin(index)
			if err != nil {
				results = append(results, result{window: window, err: err})
				continue
			}
			wg.Add(1)
			go func(window Window) {
				defer wg.Done()
				data, err := CompositeWindow(ctx, model, window, CompositeConfig{MaxFrameBytes: e.cfg.MaxFrameBytes})
				mu.Lock()
				results = append(results, result{window: window, data: data, err: err})
				mu.Unlock()
			}(window)
		}
		wg.Wait()
		sort.Slice(results, func(i, j int) bool { return results[i].window.Index < results[j].window.Index })
		for _, item := range results {
			if ctx.Err() != nil {
				if item.window.State == WindowCompositing {
					_ = e.windows.Reset(item.window.Index)
				}
				continue
			}
			if item.err != nil {
				_ = e.windows.Fail(item.window.Index, item.err)
				continue
			}
			path, err := e.frames.WriteWindow(item.data)
			if err != nil {
				_ = e.windows.Fail(item.window.Index, err)
				continue
			}
			snapshot := e.makeSnapshot(model, item.window, item.data, path)
			if err := e.store.SaveSnapshot(snapshot); err != nil {
				_ = e.windows.Fail(item.window.Index, err)
				continue
			}
			if _, err := e.windows.Commit(item.window.Index, snapshot, snapshot.CommittedSequence); err != nil {
				_ = e.windows.Fail(item.window.Index, err)
			}
		}
	}
	if err := ctx.Err(); err != nil {
		// Cancellation only discards non-persisted work. Committed windows stay
		// committed and can be reused on the next run.
		for i := 0; i < e.windows.Len(); i++ {
			window, _ := e.windows.Get(i)
			if window.State == WindowCompositing {
				_ = e.windows.Reset(i)
			}
		}
		return err
	}
	return nil
}

func (e *Engine) makeSnapshot(model *Model, window Window, result *WindowResult, path string) WindowSnapshot {
	summary := aggregateSummary(result.FrameSummaries, result.ContentHash)
	return WindowSnapshot{
		WindowIndex: window.Index, StartFrame: window.StartFrame, EndFrame: window.EndFrame,
		Layers: append([]Layer(nil), model.Layers...),
		Source: SourceSnapshot{
			Versions: append([]int(nil), model.SourceVersions...),
			TimeBase: model.TimeBase, Canvas: model.Canvas,
		},
		ModelHash: e.modelHash, SourceHash: e.sourceHash, AlgorithmVersion: AlgorithmVersion,
		FrameHash: result.FileHash, FramePath: filepath.Base(path), Summary: summary,
		FrameSummaries:    append([]FrameSummary(nil), result.FrameSummaries...),
		CommittedSequence: int64(window.Index + 1),
		CommittedAt:       time.Now().UTC().Format(time.RFC3339Nano),
	}
}

func aggregateSummary(summaries []FrameSummary, contentHash string) FrameSummary {
	if len(summaries) == 0 {
		return FrameSummary{ContentHash: contentHash}
	}
	out := FrameSummary{ContentHash: contentHash, MinLuma: summaries[0].MinLuma, MaxLuma: summaries[0].MaxLuma}
	for _, summary := range summaries {
		out.Frame += summary.Frame
		out.PixelCount += summary.PixelCount
		out.MeanLuma += summary.MeanLuma
		out.MeanAlpha += summary.MeanAlpha
		if summary.MinLuma < out.MinLuma {
			out.MinLuma = summary.MinLuma
		}
		if summary.MaxLuma > out.MaxLuma {
			out.MaxLuma = summary.MaxLuma
		}
	}
	n := float64(len(summaries))
	out.MeanLuma /= n
	out.MeanAlpha /= n
	out.Timestamp = summaries[0].Timestamp
	return out
}

func (e *Engine) setModel(model *Model, sourceHash, modelHash string) {
	e.modelMu.Lock()
	defer e.modelMu.Unlock()
	e.model = model
	e.sourceHash = sourceHash
	e.modelHash = modelHash
}

func (e *Engine) statePath() string {
	if e.cfg.StateDir == "" {
		return ""
	}
	return filepath.Join(e.cfg.StateDir, "engine-state.json")
}

func (e *Engine) pendingPath() string {
	if e.cfg.StateDir == "" {
		return ""
	}
	return filepath.Join(e.cfg.StateDir, "pending-invalidation.json")
}

func (e *Engine) persistState(model *Model) error {
	path := e.statePath()
	if path == "" {
		return nil
	}
	raw, err := CanonicalModelBytes(model)
	if err != nil {
		return err
	}
	return writeAtomic(path, raw)
}

func (e *Engine) persistPendingInvalidation(list InvalidationList) error {
	path := e.pendingPath()
	if path == "" {
		return nil
	}
	raw, err := json.MarshalIndent(list, "", "  ")
	if err != nil {
		return errCode(CodeDecode, "encode pending invalidation: %v", err)
	}
	return writeAtomic(path, append(raw, '\n'))
}

func (e *Engine) readPendingInvalidation() (InvalidationList, error) {
	path := e.pendingPath()
	if path == "" {
		return InvalidationList{}, nil
	}
	raw, err := os.ReadFile(path)
	if os.IsNotExist(err) {
		return InvalidationList{}, nil
	}
	if err != nil {
		return InvalidationList{}, errCode(CodeDecode, "read pending invalidation: %v", err)
	}
	var list InvalidationList
	if err := json.Unmarshal(raw, &list); err != nil {
		return InvalidationList{}, errCode(CodeDecode, "decode pending invalidation: %v", err)
	}
	return list, nil
}

func (e *Engine) clearPendingInvalidation() {
	if path := e.pendingPath(); path != "" {
		_ = os.Remove(path)
	}
}

func writeAtomic(path string, data []byte) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return errCode(CodeDecode, "create state directory: %v", err)
	}
	tmp, err := os.CreateTemp(filepath.Dir(path), ".state-*.tmp")
	if err != nil {
		return errCode(CodeDecode, "create state temp file: %v", err)
	}
	name := tmp.Name()
	defer os.Remove(name)
	if _, err := tmp.Write(data); err != nil {
		tmp.Close()
		return errCode(CodeDecode, "write state: %v", err)
	}
	if err := tmp.Sync(); err != nil {
		tmp.Close()
		return errCode(CodeDecode, "sync state: %v", err)
	}
	if err := tmp.Close(); err != nil {
		return errCode(CodeDecode, "close state: %v", err)
	}
	return os.Rename(name, path)
}

func (s FrameStore) Clear() error {
	entries, err := os.ReadDir(s.Dir)
	if os.IsNotExist(err) {
		return nil
	}
	if err != nil {
		return errCode(CodeDecode, "read frame store: %v", err)
	}
	for _, entry := range entries {
		if entry.IsDir() {
			continue
		}
		if filepath.Ext(entry.Name()) == ".vspw" || filepath.Ext(entry.Name()) == ".tmp" {
			if err := os.Remove(filepath.Join(s.Dir, entry.Name())); err != nil && !os.IsNotExist(err) {
				return errCode(CodeDecode, "clear frame store: %v", err)
			}
		}
	}
	return nil
}

func containsInt(values []int, target int) bool {
	for _, value := range values {
		if value == target {
			return true
		}
	}
	return false
}

func allWindowIndices(count int) []int {
	out := make([]int, count)
	for i := range out {
		out[i] = i
	}
	return out
}

func (e *Engine) ReadWindow(index int) (*WindowResult, error) {
	window, err := e.windows.Get(index)
	if err != nil {
		return nil, err
	}
	if window.State != WindowCommitted || window.Snapshot == nil {
		return nil, stateError(index, "window is not committed")
	}
	result, err := e.frames.ReadWindow(index)
	if err != nil {
		return nil, err
	}
	if result.FileHash != window.Snapshot.FrameHash || result.ContentHash != window.Snapshot.Summary.ContentHash {
		return nil, errCode(CodeDecode, "window %d cache hash no longer matches checkpoint", index)
	}
	return result, nil
}

func (e *Engine) Frame(windowIndex, frame int) (*Frame, error) {
	result, err := e.ReadWindow(windowIndex)
	if err != nil {
		return nil, err
	}
	for _, candidate := range result.Frames {
		if candidate.Frame == frame {
			return candidate, nil
		}
	}
	return nil, paraError(frame, "frame %d is not in window %d", frame, windowIndex)
}

func (e *Engine) Retry(ctx context.Context, index int) error {
	e.runMu.Lock()
	defer e.runMu.Unlock()
	if err := e.windows.Retry(index); err != nil {
		return err
	}
	return e.processWindows(ctx, e.Model(), []int{index}, true)
}
