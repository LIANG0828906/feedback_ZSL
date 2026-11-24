package engine

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

type RunMode string

const (
	ModePreview RunMode = "preview"
	ModeResume  RunMode = "resume"
	ModeFull    RunMode = "full"
)

type Controller struct {
	ctx     context.Context
	paused  atomic.Bool
	cancel  context.CancelFunc
	mu      sync.Mutex
	cond    *sync.Cond
	stopped bool
}

func NewController(ctx context.Context) (*Controller, context.Context) {
	child, cancel := context.WithCancel(ctx)
	c := &Controller{ctx: child, cancel: cancel}
	c.cond = sync.NewCond(&c.mu)
	return c, child
}

func (c *Controller) Context() context.Context {
	if c == nil || c.ctx == nil {
		return context.Background()
	}
	return c.ctx
}

func (c *Controller) Pause() {
	c.paused.Store(true)
}

func (c *Controller) Paused() bool {
	return c.paused.Load()
}

func (c *Controller) Resume() {
	c.paused.Store(false)
	c.mu.Lock()
	c.cond.Broadcast()
	c.mu.Unlock()
}

func (c *Controller) Cancel() {
	c.paused.Store(false)
	c.mu.Lock()
	c.stopped = true
	c.cond.Broadcast()
	c.mu.Unlock()
	c.cancel()
}

func (c *Controller) wait(ctx context.Context) error {
	for {
		if err := ctx.Err(); err != nil {
			return err
		}
		c.mu.Lock()
		for c.paused.Load() && !c.stopped && ctx.Err() == nil {
			c.cond.Wait()
		}
		stopped := c.stopped
		c.mu.Unlock()
		if stopped {
			return context.Canceled
		}
		if !c.paused.Load() {
			return nil
		}
	}
}

type RunOptions struct {
	Mode        RunMode
	DBPath      string
	BudgetBytes int64
	MaxWorkers  int
	BufferBytes int64
	StopAfter   int
	Controller  *Controller
	Quality     *QualityDecision
}

type BufferEvent struct {
	Frame          int    `json:"frame"`
	EstimatedBytes int64  `json:"estimatedBytes"`
	LimitBytes     int64  `json:"limitBytes"`
	Reason         string `json:"reason"`
}

type RunReport struct {
	Mode               RunMode          `json:"mode"`
	SceneID            string           `json:"sceneId"`
	Quality            QualityDecision  `json:"quality"`
	Frames             []*RenderedFrame `json:"frames"`
	CommitSequence     []int            `json:"commitSequence"`
	StateMatrix        []StateCell      `json:"stateMatrix"`
	BufferEvents       []BufferEvent    `json:"bufferEvents"`
	SnapshotStatus     SnapshotStatus   `json:"snapshotStatus"`
	Partial            bool             `json:"partial"`
	ElapsedMillis      int64            `json:"elapsedMillis"`
	EffectiveTolerance int64            `json:"effectiveTolerance"`
	EffectiveGrid      int              `json:"effectiveGrid"`
	EffectivePrecision int              `json:"effectivePrecision"`
}

// Run executes one of the three deterministic verification paths.
func Run(ctx context.Context, scene *Scene, opts RunOptions) (*RunReport, error) {
	started := time.Now()
	if scene == nil {
		return nil, &Error{Code: CodeSchema, Path: "$", Message: "nil scene"}
	}
	if opts.MaxWorkers <= 0 {
		opts.MaxWorkers = 1
	}
	if opts.MaxWorkers > 8 {
		opts.MaxWorkers = 8
	}
	if opts.BufferBytes <= 0 {
		opts.BufferBytes = 16 * 1024 * 1024
	}
	quality := QualityDecision{}
	if opts.Quality != nil {
		quality = *opts.Quality
	} else {
		var err error
		quality, err = SelectQuality(scene, opts.BudgetBytes, opts.MaxWorkers)
		if err != nil {
			return nil, err
		}
	}
	effective := ApplyQuality(scene, quality)
	if err := ValidateParameters(effective); err != nil {
		return nil, err
	}
	report := &RunReport{
		Mode:               opts.Mode,
		SceneID:            scene.ID,
		Quality:            quality,
		SnapshotStatus:     SnapshotStatus{Valid: true},
		EffectiveTolerance: effective.Tolerance,
		EffectiveGrid:      effective.Subsamples,
		EffectivePrecision: effective.Interpolation.PrecisionBits,
	}
	if opts.Mode == "" {
		opts.Mode = ModePreview
	}
	report.Mode = opts.Mode
	if opts.Mode != ModePreview && opts.DBPath == "" {
		return nil, &Error{Code: CodeSchema, Path: "dbPath", Message: "snapshot path is required for resume/full"}
	}
	runCtx := ctx
	if opts.Controller != nil {
		runCtx = opts.Controller.Context()
	}
	if opts.Controller != nil {
		_ = opts.Controller.wait(runCtx)
	}

	var store *SnapshotStore
	var err error
	if opts.Mode != ModePreview {
		store, err = OpenSnapshotStore(opts.DBPath, effective)
		if err != nil {
			return nil, err
		}
		defer store.Close()
		if opts.Mode == ModeFull {
			if err := store.Reset(scene.ID); err != nil {
				return nil, err
			}
			report.SnapshotStatus = SnapshotStatus{Valid: true, Invalidated: true, Reason: "full recompute requested"}
		}
	}
	machine := NewStageMachine(effective)
	var frames []*RenderedFrame
	resumedFrom := 0
	if opts.Mode == ModeResume {
		loaded, status, err := store.Validate()
		if err != nil {
			return nil, err
		}
		report.SnapshotStatus = status
		if status.Valid && len(loaded) > 0 {
			if !validPrefix(loaded, effective.Samples) {
				if err := store.Reset(scene.ID); err != nil {
					return nil, err
				}
				report.SnapshotStatus = SnapshotStatus{Valid: true, Invalidated: true, Reason: "snapshot is not a stable time prefix"}
				loaded = nil
			}
		}
		frames = loaded
		resumedFrom = len(loaded)
	}
	for _, f := range frames {
		if err := markCommitted(machine, effective, f); err != nil {
			return nil, err
		}
		report.CommitSequence = append(report.CommitSequence, f.CommitSequence)
	}
	report.Frames = append([]*RenderedFrame(nil), frames...)
	missing := make([]int, 0)
	for i := len(frames); i < len(effective.Samples); i++ {
		missing = append(missing, i)
	}
	if len(missing) == 0 {
		sort.Slice(frames, func(i, j int) bool { return frames[i].CommitSequence < frames[j].CommitSequence })
		report.Frames = frames
		report.SnapshotStatus.FrameCount = len(frames)
		report.SnapshotStatus.ResumedFrom = resumedFrom
		report.StateMatrix = machine.Matrix()
		report.ElapsedMillis = time.Since(started).Milliseconds()
		return report, nil
	}
	if opts.Mode == ModePreview {
		frames, err = runPreview(runCtx, effective, quality, missing, opts, machine, report)
	} else {
		frames, err = runParallel(runCtx, effective, quality, missing, len(frames), opts, machine, report, store)
	}
	if frames == nil {
		frames = report.Frames
	}
	sort.Slice(frames, func(i, j int) bool { return frames[i].CommitSequence < frames[j].CommitSequence })
	report.Frames = frames
	for _, f := range frames {
		found := false
		for _, seq := range report.CommitSequence {
			if seq == f.CommitSequence {
				found = true
				break
			}
		}
		if !found {
			report.CommitSequence = append(report.CommitSequence, f.CommitSequence)
		}
	}
	sort.Ints(report.CommitSequence)
	report.StateMatrix = machine.Matrix()
	report.ElapsedMillis = time.Since(started).Milliseconds()
	if opts.Mode == ModeResume {
		report.SnapshotStatus.FrameCount = len(report.Frames)
		report.SnapshotStatus.ResumedFrom = resumedFrom
	} else if opts.Mode == ModeFull {
		report.SnapshotStatus.FrameCount = len(report.Frames)
	}
	if err != nil {
		report.Partial = true
		return report, err
	}
	return report, nil
}

func validPrefix(frames []*RenderedFrame, samples []int64) bool {
	if len(frames) > len(samples) {
		return false
	}
	for i, f := range frames {
		if f.Time != samples[i] || f.CommitSequence != i {
			return false
		}
	}
	return true
}

func markCommitted(machine *StageMachine, scene *Scene, frame *RenderedFrame) error {
	for _, p := range scene.Paths {
		for _, stage := range []Stage{StageFlatten, StageCoverage, StageComposite} {
			_ = machine.Transition(frame.Time, p.ID, stage, StateProcessing)
			if err := machine.Transition(frame.Time, p.ID, stage, StateCommitted); err != nil {
				return err
			}
		}
	}
	return nil
}

func runPreview(ctx context.Context, scene *Scene, quality QualityDecision, missing []int, opts RunOptions, machine *StageMachine, report *RunReport) ([]*RenderedFrame, error) {
	out := append([]*RenderedFrame(nil), report.Frames...)
	for _, idx := range missing {
		if err := ctx.Err(); err != nil {
			return out, err
		}
		if opts.Controller != nil {
			if err := opts.Controller.wait(ctx); err != nil {
				return out, err
			}
		}
		frame, err := renderWithStates(ctx, scene, quality, scene.Samples[idx], idx, len(out), machine)
		if err != nil {
			return out, err
		}
		frame.CommitSequence = len(out)
		out = append(out, frame)
		report.CommitSequence = append(report.CommitSequence, frame.CommitSequence)
		if opts.StopAfter > 0 && len(out) >= opts.StopAfter {
			return out, context.Canceled
		}
	}
	return out, nil
}

func runParallel(ctx context.Context, scene *Scene, quality QualityDecision, missing []int, startSeq int, opts RunOptions, machine *StageMachine, report *RunReport, store *SnapshotStore) ([]*RenderedFrame, error) {
	existing := append([]*RenderedFrame(nil), report.Frames...)
	if opts.StopAfter > 0 && len(existing) >= opts.StopAfter {
		return existing, context.Canceled
	}
	workers := opts.MaxWorkers
	if workers > len(missing) {
		workers = len(missing)
	}
	if workers < 1 {
		return existing, nil
	}
	type job struct {
		idx    int
		sample int64
	}
	type result struct {
		frame *RenderedFrame
		err   error
	}
	jobs := make(chan job)
	results := make(chan result, workers)
	var oversizeMu sync.Mutex
	var bufferMu sync.Mutex
	var bufferEvents []BufferEvent
	var wg sync.WaitGroup
	for w := 0; w < workers; w++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for j := range jobs {
				if err := ctx.Err(); err != nil {
					results <- result{err: err}
					continue
				}
				if opts.Controller != nil {
					if err := opts.Controller.wait(ctx); err != nil {
						results <- result{err: err}
						continue
					}
				}
				estimate := estimatedFrameBytes(scene)
				if estimate > opts.BufferBytes {
					oversizeMu.Lock()
					bufferMu.Lock()
					bufferEvents = append(bufferEvents, BufferEvent{
						Frame: j.idx, EstimatedBytes: estimate, LimitBytes: opts.BufferBytes,
						Reason: "per-worker buffer exhausted; blocked and serialized",
					})
					bufferMu.Unlock()
					frame, err := renderWithStates(ctx, scene, quality, j.sample, j.idx, -1, machine)
					oversizeMu.Unlock()
					results <- result{frame: frame, err: err}
					continue
				}
				frame, err := renderWithStates(ctx, scene, quality, j.sample, j.idx, -1, machine)
				results <- result{frame: frame, err: err}
			}
		}()
	}
	pending := make(map[int]*RenderedFrame)
	nextCommit := startSeq
	dispatched := 0
	received := 0
	var firstErr error
	dispatch := func() bool {
		if dispatched >= len(missing) {
			return false
		}
		idx := missing[dispatched]
		select {
		case jobs <- job{idx: idx, sample: scene.Samples[idx]}:
			dispatched++
			return true
		case <-ctx.Done():
			return false
		}
	}
	for i := 0; i < workers && i < len(missing); i++ {
		if !dispatch() {
			break
		}
	}
	for received < dispatched {
		res := <-results
		received++
		if res.err != nil {
			if firstErr == nil && res.err != context.Canceled {
				firstErr = res.err
			}
			continue
		}
		res.frame.CommitSequence = res.frame.Index
		pending[res.frame.Index] = res.frame
		for {
			frame, ok := pending[nextCommit]
			if !ok {
				break
			}
			if err := ctx.Err(); err != nil && len(existing) == 0 {
				firstErr = err
				break
			}
			if err := store.SaveFrame(frame); err != nil {
				firstErr = err
				break
			}
			existing = append(existing, frame)
			report.CommitSequence = append(report.CommitSequence, frame.CommitSequence)
			delete(pending, nextCommit)
			nextCommit++
			if opts.StopAfter > 0 && len(existing) >= opts.StopAfter && firstErr == nil {
				firstErr = context.Canceled
				break
			}
		}
		if firstErr != nil {
			break
		}
		if dispatched < len(missing) {
			_ = dispatch()
		}
	}
	close(jobs)
	go func() {
		wg.Wait()
		close(results)
	}()
	for range results {
	}
	sort.Slice(bufferEvents, func(i, j int) bool { return bufferEvents[i].Frame < bufferEvents[j].Frame })
	report.BufferEvents = append(report.BufferEvents, bufferEvents...)
	if ctx.Err() != nil && firstErr == nil {
		firstErr = ctx.Err()
	}
	if firstErr != nil {
		return existing, firstErr
	}
	if len(existing) != len(scene.Samples) {
		return existing, &Error{Code: CodeState, Path: "commit", Message: fmt.Sprintf("expected %d committed frames, got %d", len(scene.Samples), len(existing))}
	}
	return existing, nil
}

func renderWithStates(ctx context.Context, scene *Scene, quality QualityDecision, t int64, index, seq int, machine *StageMachine) (*RenderedFrame, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	for _, p := range scene.Paths {
		for _, stage := range []Stage{StageFlatten, StageCoverage, StageComposite} {
			if err := machine.Transition(t, p.ID, stage, StateProcessing); err != nil {
				return nil, err
			}
		}
	}
	frame, err := RenderFrame(scene, t, index, quality)
	if err != nil {
		for _, p := range scene.Paths {
			for _, stage := range []Stage{StageFlatten, StageCoverage, StageComposite} {
				if machine.StateOf(t, p.ID, stage) == StateProcessing {
					_ = machine.Transition(t, p.ID, stage, StateFailed)
				}
			}
		}
		return nil, err
	}
	for _, p := range scene.Paths {
		for _, stage := range []Stage{StageFlatten, StageCoverage, StageComposite} {
			if err := machine.Transition(t, p.ID, stage, StateCommitted); err != nil {
				return nil, err
			}
		}
	}
	frame.CommitSequence = seq
	return frame, nil
}

func estimatedFrameBytes(scene *Scene) int64 {
	stats := StatsOf(scene)
	return stats.EstPixels*4*int64(stats.PathCount+2) + int64(scene.Subsamples*scene.Subsamples*8)
}

// WriteRunArtifacts persists PNGs plus a deterministic JSON report.
func WriteRunArtifacts(report *RunReport, dir string) error {
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		return err
	}
	for _, entry := range entries {
		name := entry.Name()
		if strings.HasSuffix(name, ".png") || name == "commits.json" {
			if err := os.Remove(filepath.Join(dir, name)); err != nil {
				return err
			}
		}
	}
	for i, f := range report.Frames {
		data, err := f.PNG()
		if err != nil {
			return err
		}
		name := fmt.Sprintf("%03d_%dms.png", i, f.TimeMs)
		if err := os.WriteFile(filepath.Join(dir, name), data, 0o644); err != nil {
			return err
		}
	}
	meta := struct {
		Mode           RunMode  `json:"mode"`
		CommitSequence []int    `json:"commitSequence"`
		FrameHashes    []string `json:"frameHashes"`
		Partial        bool     `json:"partial"`
	}{
		Mode: report.Mode, CommitSequence: report.CommitSequence, Partial: report.Partial,
	}
	for _, f := range report.Frames {
		meta.FrameHashes = append(meta.FrameHashes, f.ContentHash)
	}
	data, _ := jsonMarshalIndent(meta)
	return os.WriteFile(filepath.Join(dir, "commits.json"), data, 0o644)
}

func jsonMarshalIndent(v any) ([]byte, error) {
	return json.MarshalIndent(v, "", "  ")
}
