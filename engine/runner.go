package engine

import (
	"context"
	"fmt"
	"runtime/debug"
	"sort"
	"sync"

	"virtualstudio/compose"
	"virtualstudio/model"
	"virtualstudio/store"
)

type RunOptions struct {
	Mode                      string
	MaxWorkers                int
	PixelBufferBytesPerWorker int64
	PixelBufferTotalBytes     int64
	ForceFull                 bool
	MaxRetries                int
	SuspendAfter              int
	CancelAfter               int
	Samples                   []model.SamplePoint
	Faults                    map[int]error
}

type WindowRun struct {
	Index    int                   `json:"index"`
	Start    int                   `json:"start_frame"`
	End      int                   `json:"end_frame"`
	State    WindowState           `json:"state"`
	Reused   bool                  `json:"reused"`
	Frames   []model.Frame         `json:"frames"`
	Snapshot *model.WindowSnapshot `json:"snapshot,omitempty"`
	Error    string                `json:"error,omitempty"`
}

type Progress struct {
	Committed int `json:"committed"`
	Total     int `json:"total"`
}

type RunResult struct {
	Mode                    string         `json:"mode"`
	ProjectID               string         `json:"project_id"`
	SourceVersion           string         `json:"source_version"`
	WindowSize              int            `json:"window_size"`
	Frames                  []model.Frame  `json:"frames"`
	Windows                 []WindowRun    `json:"windows"`
	CommitSequence          []int          `json:"commit_sequence"`
	ReusedWindows           []int          `json:"reused_windows"`
	RecomputedWindows       []int          `json:"recomputed_windows"`
	FailedWindows           map[int]string `json:"failed_windows,omitempty"`
	Progress                Progress       `json:"progress"`
	CacheInvalidated        bool           `json:"cache_invalidated"`
	CacheInvalidationReason string         `json:"cache_invalidation_reason,omitempty"`
	BufferEvents            []BufferEvent  `json:"buffer_events,omitempty"`
	Suspended               bool           `json:"suspended"`
	Canceled                bool           `json:"canceled"`
}

func (w *Workspace) Run(ctx context.Context, opts RunOptions) (*RunResult, error) {
	if opts.Mode == "" {
		opts.Mode = "realtime"
	}
	if opts.MaxWorkers <= 0 {
		opts.MaxWorkers = 3
	}
	if opts.MaxRetries < 0 {
		opts.MaxRetries = 0
	}
	if opts.Samples == nil {
		opts.Samples = compose.DefaultSamples(w.scene.Width, w.scene.Height)
	}
	for i, point := range opts.Samples {
		if point.X < 0 || point.X >= w.scene.Width || point.Y < 0 || point.Y >= w.scene.Height {
			return nil, model.NewError(model.CodeParam, fmt.Sprintf("sample_points[%d]", i), "sample is outside the frame")
		}
	}
	need := w.scene.FrameBytes() * 9
	if need < 4096 {
		need = 4096
	}
	if opts.PixelBufferBytesPerWorker <= 0 {
		opts.PixelBufferBytesPerWorker = need * 2
		if opts.PixelBufferBytesPerWorker < 1<<20 {
			opts.PixelBufferBytesPerWorker = 1 << 20
		}
	}
	if need > opts.PixelBufferBytesPerWorker {
		return nil, model.NewError(model.CodeParam, "pixel_buffer_bytes_per_worker",
			fmt.Sprintf("window needs %d bytes, limit is %d", need, opts.PixelBufferBytesPerWorker))
	}
	if opts.PixelBufferTotalBytes > 0 && need > opts.PixelBufferTotalBytes {
		return nil, model.NewError(model.CodeParam, "pixel_buffer_total_bytes",
			fmt.Sprintf("window needs %d bytes, total budget is %d", need, opts.PixelBufferTotalBytes))
	}
	if opts.MaxWorkers > w.windows.Total() {
		opts.MaxWorkers = w.windows.Total()
	}
	limiter, err := NewBufferLimiterWithTotal(opts.MaxWorkers, opts.PixelBufferBytesPerWorker, opts.PixelBufferTotalBytes)
	if err != nil {
		return nil, err
	}
	result := &RunResult{
		Mode:          opts.Mode,
		ProjectID:     w.projectID,
		SourceVersion: w.scene.SourceVersion,
		WindowSize:    w.windowFrames,
		FailedWindows: map[int]string{},
	}
	if err := ctx.Err(); err != nil {
		result.Canceled = true
		result.Suspended = true
		w.windows, _ = NewWindowSet(w.scene.FrameCount, w.windowFrames)
		result.Progress = Progress{Committed: 0, Total: w.windows.Total()}
		w.complete = false
		_ = w.store.SetComplete(context.WithoutCancel(ctx), w.projectID, false)
		return result, nil
	}
	w.windows, _ = NewWindowSet(w.scene.FrameCount, w.windowFrames)
	cache, invalidated, reason, err := w.loadValidCache(ctx, opts.ForceFull, opts.Samples)
	if err != nil {
		return nil, err
	}
	result.CacheInvalidated = invalidated
	result.CacheInvalidationReason = reason
	windowRuns := make(map[int]*WindowRun, w.windows.Total())
	for index, cp := range cache {
		win, ok := w.windows.Window(index)
		if !ok {
			continue
		}
		if err := win.RestoreCommitted(cp.Snapshot); err != nil {
			return nil, err
		}
		frames := cloneFrames(cp.Frames)
		windowRuns[index] = &WindowRun{
			Index: index, Start: win.StartFrame(), End: win.EndFrame(), State: StateCommitted,
			Reused: true, Frames: frames, Snapshot: cp.Snapshot.Clone(),
		}
	}
	next := 0
	for next < w.windows.Total() {
		if run, ok := windowRuns[next]; ok {
			result.ReusedWindows = append(result.ReusedWindows, next)
			result.CommitSequence = append(result.CommitSequence, next)
			result.Frames = append(result.Frames, cloneFrames(run.Frames)...)
			next++
			if opts.SuspendAfter > 0 && len(result.CommitSequence) >= opts.SuspendAfter {
				result.Suspended = true
				break
			}
			if opts.CancelAfter > 0 && len(result.CommitSequence) >= opts.CancelAfter {
				result.Suspended = true
				result.Canceled = true
				break
			}
			continue
		}
		if err := ctx.Err(); err != nil {
			result.Canceled = true
			result.Suspended = true
			break
		}
		end := next + opts.MaxWorkers
		if end > w.windows.Total() {
			end = w.windows.Total()
		}
		for i := next; i < end; i++ {
			if _, cached := windowRuns[i]; cached {
				end = i
				break
			}
		}
		jobs := make([]composeJob, 0, end-next)
		for i := next; i < end; i++ {
			win, _ := w.windows.Window(i)
			jobs = append(jobs, composeJob{index: i, win: win})
		}
		computed := w.composeBatch(ctx, jobs, limiter, opts, need)
		sort.Slice(computed, func(i, j int) bool { return computed[i].index < computed[j].index })
		stop := false
		for _, comp := range computed {
			win, _ := w.windows.Window(comp.index)
			if comp.err != nil {
				result.FailedWindows[comp.index] = comp.err.Error()
				windowRuns[comp.index] = &WindowRun{
					Index: comp.index, Start: win.StartFrame(), End: win.EndFrame(),
					State: win.State(), Error: comp.err.Error(),
				}
				for _, rest := range computed {
					if rest.index > comp.index && rest.err == nil {
						if restWin, ok := w.windows.Window(rest.index); ok && restWin.State() == StateCompositing {
							_ = restWin.Abort("peer window failed before commit boundary")
						}
					}
				}
				stop = true
				break
			}
			snapshot := model.BuildWindowSnapshot(w.scene, w.commands, comp.index, win.StartFrame(), win.EndFrame())
			if snapshot.ContentHash != comp.snapshot.ContentHash || snapshot.ParamVersion != comp.snapshot.ParamVersion {
				_ = win.Fail("window snapshot changed during composition")
				result.FailedWindows[comp.index] = "window snapshot changed during composition"
				windowRuns[comp.index] = &WindowRun{
					Index: comp.index, Start: win.StartFrame(), End: win.EndFrame(),
					State: win.State(), Error: "window snapshot changed during composition",
				}
				for _, rest := range computed {
					if rest.index > comp.index && rest.err == nil {
						if restWin, ok := w.windows.Window(rest.index); ok && restWin.State() == StateCompositing {
							_ = restWin.Abort("peer window failed before commit boundary")
						}
					}
				}
				stop = true
				break
			}
			cp := store.Checkpoint{
				ProjectID:        w.projectID,
				WindowIndex:      comp.index,
				StartFrame:       win.StartFrame(),
				EndFrame:         win.EndFrame(),
				Sequence:         comp.index,
				ParamVersion:     snapshot.ParamVersion,
				ContentHash:      snapshot.ContentHash,
				FrameDigest:      compose.WindowDigest(comp.frames),
				SourceVersion:    w.scene.SourceVersion,
				AlgorithmVersion: model.AlgorithmVersion,
				Snapshot:         snapshot,
				Frames:           comp.frames,
			}
			commitCtx := context.WithoutCancel(ctx)
			if err := w.store.PutCheckpoint(commitCtx, cp); err != nil {
				_ = win.Fail("checkpoint write failed: " + err.Error())
				return nil, err
			}
			if err := win.Commit(snapshot); err != nil {
				return nil, err
			}
			windowRuns[comp.index] = &WindowRun{
				Index: comp.index, Start: win.StartFrame(), End: win.EndFrame(), State: StateCommitted,
				Reused: false, Frames: cloneFrames(comp.frames), Snapshot: snapshot.Clone(),
			}
			result.RecomputedWindows = append(result.RecomputedWindows, comp.index)
			result.CommitSequence = append(result.CommitSequence, comp.index)
			result.Frames = append(result.Frames, cloneFrames(comp.frames)...)
			if opts.SuspendAfter > 0 && len(result.CommitSequence) >= opts.SuspendAfter {
				result.Suspended = true
				for _, rest := range computed {
					if rest.index > comp.index && rest.err == nil {
						if restWin, ok := w.windows.Window(rest.index); ok && restWin.State() == StateCompositing {
							_ = restWin.Abort("suspended before commit boundary")
						}
					}
				}
				stop = true
				break
			}
			if opts.CancelAfter > 0 && len(result.CommitSequence) >= opts.CancelAfter {
				result.Suspended = true
				result.Canceled = true
				for _, rest := range computed {
					if rest.index > comp.index && rest.err == nil {
						if restWin, ok := w.windows.Window(rest.index); ok && restWin.State() == StateCompositing {
							_ = restWin.Abort("canceled before commit boundary")
						}
					}
				}
				stop = true
				break
			}
		}
		if stop {
			break
		}
		next = end
		if err := ctx.Err(); err != nil {
			result.Canceled = true
			result.Suspended = true
			break
		}
	}
	result.BufferEvents = limiter.Events()
	sort.Slice(result.BufferEvents, func(i, j int) bool {
		if result.BufferEvents[i].Window == result.BufferEvents[j].Window {
			return result.BufferEvents[i].Worker < result.BufferEvents[j].Worker
		}
		return result.BufferEvents[i].Window < result.BufferEvents[j].Window
	})
	for i := 0; i < w.windows.Total(); i++ {
		if run, ok := windowRuns[i]; ok {
			result.Windows = append(result.Windows, *run)
			continue
		}
		if win, ok := w.windows.Window(i); ok {
			result.Windows = append(result.Windows, WindowRun{
				Index: i, Start: win.StartFrame(), End: win.EndFrame(), State: win.State(),
			})
		}
	}
	sort.Slice(result.Windows, func(i, j int) bool { return result.Windows[i].Index < result.Windows[j].Index })
	result.Progress = Progress{Committed: w.windows.Committed(), Total: w.windows.Total()}
	complete := result.Progress.Committed == result.Progress.Total
	if err := w.store.SetComplete(context.WithoutCancel(ctx), w.projectID, complete); err != nil {
		return nil, err
	}
	w.complete = complete
	return result, nil
}

type windowComputation struct {
	index    int
	frames   []model.Frame
	snapshot *model.WindowSnapshot
	err      error
}

type composeJob struct {
	index int
	win   *Window
}

func (w *Workspace) composeBatch(parent context.Context, jobs []composeJob, limiter *BufferLimiter, opts RunOptions, need int64) []windowComputation {
	results := make(chan windowComputation, len(jobs))
	var wg sync.WaitGroup
	workerCtx := context.WithoutCancel(parent)
	for workerID, job := range jobs {
		wg.Add(1)
		go func(workerID int, index int, win *Window) {
			defer wg.Done()
			results <- w.composeOne(workerCtx, workerID, index, win, limiter, opts, need)
		}(workerID, job.index, job.win)
	}
	wg.Wait()
	close(results)
	out := make([]windowComputation, 0, len(jobs))
	for item := range results {
		out = append(out, item)
	}
	return out
}

func (w *Workspace) composeOne(ctx context.Context, workerID, index int, win *Window, limiter *BufferLimiter, opts RunOptions, need int64) (out windowComputation) {
	out.index = index
	defer func() {
		if recovered := recover(); recovered != nil {
			out.err = model.NewError(model.CodeDecode, fmt.Sprintf("windows[%d]", index),
				fmt.Sprintf("worker panic isolated: %v\n%s", recovered, debug.Stack()))
			if win.State() == StateCompositing {
				_ = win.Fail(out.err.Error())
			}
		}
	}()
	for attempt := 0; attempt <= opts.MaxRetries; attempt++ {
		if win.State() == StatePending {
			if err := win.Begin(); err != nil {
				out.err = err
				return out
			}
		}
		release, err := limiter.Acquire(ctx, need, index, workerID)
		if err != nil {
			out.err = err
			if win.State() == StateCompositing {
				_ = win.Fail(err.Error())
			}
			return out
		}
		frames, renderErr := compose.RenderWindow(w.scene, w.commands, win.StartFrame(), win.EndFrame(), compose.RenderOptions{Samples: opts.Samples})
		release()
		if injected, ok := opts.Faults[index]; ok {
			renderErr = injected
		}
		if renderErr == nil {
			out.frames = frames
			out.snapshot = model.BuildWindowSnapshot(w.scene, w.commands, index, win.StartFrame(), win.EndFrame())
			return out
		}
		out.err = renderErr
		if win.State() == StateCompositing {
			_ = win.Fail(renderErr.Error())
		}
		if attempt < opts.MaxRetries {
			if err := win.Retry(); err != nil {
				return out
			}
			out.err = nil
		}
	}
	return out
}

func (w *Workspace) loadValidCache(ctx context.Context, forceFull bool, samples []model.SamplePoint) (map[int]store.Checkpoint, bool, string, error) {
	if forceFull {
		w.complete = false
		if err := w.store.Purge(ctx, w.projectID); err != nil {
			return nil, false, "", err
		}
		if err := w.persistProject(ctx); err != nil {
			return nil, false, "", err
		}
		return map[int]store.Checkpoint{}, true, "full recompute requested", nil
	}
	checkpoints, err := w.store.LoadCheckpoints(ctx, w.projectID)
	if err != nil {
		if invalid, ok := err.(*store.InvalidatedError); ok {
			w.complete = false
			if persistErr := w.persistProject(ctx); persistErr != nil {
				return nil, false, "", persistErr
			}
			return map[int]store.Checkpoint{}, true, invalid.Reason, nil
		}
		return nil, false, "", err
	}
	cache := make(map[int]store.Checkpoint, len(checkpoints))
	for _, cp := range checkpoints {
		win, ok := w.windows.Window(cp.WindowIndex)
		if !ok {
			w.complete = false
			if err := w.store.Purge(ctx, w.projectID); err != nil {
				return nil, false, "", err
			}
			if err := w.persistProject(ctx); err != nil {
				return nil, false, "", err
			}
			return map[int]store.Checkpoint{}, true, fmt.Sprintf("window %d is outside the current project", cp.WindowIndex), nil
		}
		expected := model.BuildWindowSnapshot(w.scene, w.commands, cp.WindowIndex, win.StartFrame(), win.EndFrame())
		if cp.ContentHash != expected.ContentHash || cp.ParamVersion != expected.ParamVersion ||
			cp.SourceVersion != w.scene.SourceVersion || cp.StartFrame != win.StartFrame() || cp.EndFrame != win.EndFrame() {
			w.complete = false
			reason := fmt.Sprintf("window %d content or parameter version does not match the current project", cp.WindowIndex)
			if err := w.store.Purge(ctx, w.projectID); err != nil {
				return nil, false, "", err
			}
			if err := w.persistProject(ctx); err != nil {
				return nil, false, "", err
			}
			return map[int]store.Checkpoint{}, true, reason, nil
		}
		if !cachedSamplesMatch(cp, samples) {
			w.complete = false
			if err := w.store.Purge(ctx, w.projectID); err != nil {
				return nil, false, "", err
			}
			if err := w.persistProject(ctx); err != nil {
				return nil, false, "", err
			}
			return map[int]store.Checkpoint{}, true, fmt.Sprintf("window %d sample points differ from the current run", cp.WindowIndex), nil
		}
		cache[cp.WindowIndex] = cp
	}
	return cache, false, "", nil
}

func cachedSamplesMatch(cp store.Checkpoint, samples []model.SamplePoint) bool {
	if len(cp.Frames) == 0 {
		return len(samples) == 0
	}
	for _, frame := range cp.Frames {
		if len(frame.Stats.Samples) != len(samples) {
			return false
		}
		for i, sample := range samples {
			if frame.Stats.Samples[i].X != sample.X || frame.Stats.Samples[i].Y != sample.Y {
				return false
			}
		}
	}
	return true
}

func cloneFrames(frames []model.Frame) []model.Frame {
	out := make([]model.Frame, len(frames))
	for i := range frames {
		out[i] = frames[i].Clone()
	}
	return out
}
