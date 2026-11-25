package pipeline

import (
	"context"
	"encoding/json"
	"fmt"
	"runtime/debug"
	"sort"
	"sync"
	"time"

	"qualitybench/internal/checkpoint"
	"qualitybench/internal/core"
	"qualitybench/internal/model"
	"qualitybench/internal/ops"
	"qualitybench/internal/quality"
)

type Mode string

const (
	ModeLive   Mode = "live"
	ModeResume Mode = "resume"
	ModeFull   Mode = "full"
)

type Options struct {
	RunID                string
	ImageID              string
	ImageSeed            int64
	ProjectFingerprint   string
	TruthFingerprint     string
	AlgorithmVersion     string
	BlockSize            int
	Workers              int
	MaxThreadBufferBytes int64
	BufferCapacityBytes  int64
	PersistentCache      bool
	StopAfter            int
	TaskDelay            time.Duration
	Store                checkpoint.Store
	Fault                func(step int, block core.Rect) error
	OnStepComplete       func(step int, frame *core.Frame)
	OnProgress           func(completed, total int)
	AttributionShare     float64
	disableCache         bool
}

type CacheStats struct {
	Hits           int `json:"hits"`
	Misses         int `json:"misses"`
	PersistentHits int `json:"persistent_hits"`
	LoadedReused   int `json:"loaded_reused"`
	Unaffected     int `json:"unaffected_reused"`
}

type StepTrace struct {
	Step          int       `json:"step"`
	Type          string    `json:"type"`
	InputRegion   core.Rect `json:"input_region"`
	ActionRegion  core.Rect `json:"action_region"`
	OutputRegion  core.Rect `json:"output_region"`
	InputDigest   string    `json:"input_digest"`
	OutputDigest  string    `json:"output_digest"`
	SupportRadius int       `json:"support_radius"`
	BlockCount    int       `json:"block_count"`
}

type Result struct {
	RunID             string                    `json:"run_id"`
	ImageID           string                    `json:"image_id"`
	Mode              Mode                      `json:"mode"`
	FallbackFull      bool                      `json:"fallback_full"`
	Invalidated       bool                      `json:"invalidated"`
	InvalidatedReason string                    `json:"invalidated_reason,omitempty"`
	Completed         bool                      `json:"completed"`
	Interrupted       bool                      `json:"interrupted"`
	Status            string                    `json:"status"`
	ChainFingerprint  string                    `json:"chain_fingerprint"`
	BlockSize         int                       `json:"block_size"`
	StepCount         int                       `json:"step_count"`
	StepFrames        []*core.Frame             `json:"-"`
	StepTraces        []StepTrace               `json:"step_traces"`
	Evaluations       []quality.BlockEvaluation `json:"evaluations"`
	Cache             CacheStats                `json:"cache"`
	BufferWaits       []WaitEvent               `json:"buffer_waits"`
	QueueWaits        []WaitEvent               `json:"queue_waits"`
	StepStates        []core.State              `json:"step_states"`
	Elapsed           time.Duration             `json:"elapsed_ns"`
	VersionID         string                    `json:"version_id,omitempty"`
}

type Invalidation struct {
	Step   int            `json:"step"`
	Block  *core.BlockRef `json:"block,omitempty"`
	Reason string         `json:"reason"`
}

type EditResult struct {
	VersionID        string                    `json:"version_id"`
	ChainFingerprint string                    `json:"chain_fingerprint"`
	Invalidations    []Invalidation            `json:"invalidations"`
	RecomputedSteps  int                       `json:"recomputed_steps"`
	RecomputedBlocks int                       `json:"recomputed_blocks"`
	Cache            CacheStats                `json:"cache"`
	Evaluations      []quality.BlockEvaluation `json:"evaluations"`
	UnaffectedReused int                       `json:"unaffected_reused"`
	FinalFrameHash   string                    `json:"final_frame_hash"`
}

type Version struct {
	ID          string
	Chain       model.Chain
	Frames      []*core.Frame
	Keys        [][]string
	InputHashes [][]string
	Evaluations []quality.BlockEvaluation
	Cache       CacheStats
}

type blockTask struct {
	order       int
	step        int
	blockIndex  int
	rect        core.Rect
	prev        *core.Frame
	action      core.Rect
	inputRegion core.Rect
	inputHash   string
	cacheKey    string
	upstreamKey string
	queuedAt    time.Time
	resultCh    chan taskResult
	ctx         context.Context
	control     *Control
}

type taskResult struct {
	task       *blockTask
	pixels     []byte
	cacheHit   bool
	persistent bool
	queueWait  time.Duration
	bufferWait []WaitEvent
	err        error
}

type workerPool struct {
	engine *Engine
	tasks  chan *blockTask
	wg     sync.WaitGroup
	closed bool
}

func newWorkerPool(e *Engine, workers int) *workerPool {
	p := &workerPool{engine: e, tasks: make(chan *blockTask, max(2, workers*2))}
	for i := 0; i < workers; i++ {
		p.wg.Add(1)
		go p.worker()
	}
	return p
}

func (p *workerPool) worker() {
	defer p.wg.Done()
	for task := range p.tasks {
		res := p.engine.executeTask(task)
		task.resultCh <- res
	}
}

func (p *workerPool) Close() {
	if p.closed {
		return
	}
	p.closed = true
	close(p.tasks)
	p.wg.Wait()
}

type Engine struct {
	baseline *core.Frame
	chain    model.Chain
	opts     Options
	grid     []core.Rect
	index    map[string]int
	store    checkpoint.Store

	frames      []*core.Frame
	keys        [][]string
	inputHashes [][]string
	cache       *MemoryCache
	buffers     *BufferPool

	stateMu        sync.Mutex
	controlMu      sync.RWMutex
	activeControl  *Control
	pauseRequested bool
	stepMachines   []*core.Machine
	blockMachines  [][]*core.Machine

	evals    []quality.BlockEvaluation
	traces   []StepTrace
	versions []Version
	revision int
	ran      bool
}

func New(baseline *core.Frame, chain model.Chain, opts Options) (*Engine, error) {
	if baseline == nil {
		return nil, core.Schema("baseline", "baseline frame is missing")
	}
	if err := baseline.Validate(); err != nil {
		return nil, core.Range("baseline", "%v", err)
	}
	if opts.Store == nil {
		return nil, core.Schema("store", "checkpoint store is required")
	}
	if opts.RunID == "" || opts.ImageID == "" {
		return nil, core.Schema("run_id", "run id and image id are required")
	}
	if opts.BlockSize <= 0 || baseline.Width%opts.BlockSize != 0 || baseline.Height%opts.BlockSize != 0 {
		e := core.BlockError("block size %d must divide %dx%d", opts.BlockSize, baseline.Width, baseline.Height)
		e.Details = map[string]any{"candidates": model.CommonCandidates(baseline.Width, baseline.Height)}
		return nil, e
	}
	if err := model.ValidateSteps(chain.Steps, baseline.Width, baseline.Height); err != nil {
		return nil, err
	}
	if opts.Workers <= 0 {
		opts.Workers = 1
	}
	if opts.AlgorithmVersion == "" {
		opts.AlgorithmVersion = ops.AlgorithmVersion
	}
	if opts.AttributionShare <= 0 || opts.AttributionShare > 1 {
		opts.AttributionShare = 0.22
	}
	grid := checkpoint.Grid(baseline.Width, baseline.Height, opts.BlockSize)
	required := fullFrameScratch(baseline)
	if opts.MaxThreadBufferBytes > 0 && required > opts.MaxThreadBufferBytes {
		e := core.Para(0, "max_thread_buffer_bytes",
			"block scratch need %d exceeds per-thread limit %d", required, opts.MaxThreadBufferBytes)
		e.Details = map[string]any{"required_bytes": required}
		return nil, e
	}
	if opts.BufferCapacityBytes <= 0 {
		if opts.MaxThreadBufferBytes > 0 {
			opts.BufferCapacityBytes = opts.MaxThreadBufferBytes * int64(max(2, opts.Workers))
		} else {
			opts.BufferCapacityBytes = required * int64(max(2, opts.Workers))
		}
	}
	n := len(chain.Steps)
	eng := &Engine{
		baseline:    baseline.Clone(),
		chain:       chain.Clone(),
		opts:        opts,
		grid:        grid,
		index:       make(map[string]int, len(grid)),
		store:       opts.Store,
		frames:      make([]*core.Frame, n+1),
		keys:        make([][]string, n+1),
		inputHashes: make([][]string, n+1),
		cache:       NewMemoryCache(),
		buffers:     NewBufferPool(opts.BufferCapacityBytes),
	}
	for i, block := range grid {
		eng.index[fmt.Sprintf("%d,%d", block.X, block.Y)] = i
	}
	for i := 0; i <= n; i++ {
		eng.keys[i] = make([]string, len(grid))
		eng.inputHashes[i] = make([]string, len(grid))
	}
	eng.frames[0] = eng.baseline
	eng.initMachines()
	return eng, nil
}

func (e *Engine) Chain() model.Chain {
	return e.chain.Clone()
}

func (e *Engine) Frames() []*core.Frame {
	return append([]*core.Frame(nil), e.frames...)
}

func (e *Engine) SetFault(fn func(step int, block core.Rect) error) {
	e.opts.Fault = fn
}

func (e *Engine) Pause() {
	e.controlMu.Lock()
	c := e.activeControl
	e.pauseRequested = true
	e.controlMu.Unlock()
	if c != nil {
		c.Pause()
	}
}

func (e *Engine) Resume() {
	e.controlMu.Lock()
	c := e.activeControl
	e.pauseRequested = false
	e.controlMu.Unlock()
	if c != nil {
		c.Resume()
	}
}

func (e *Engine) Cancel() {
	e.controlMu.RLock()
	c := e.activeControl
	e.controlMu.RUnlock()
	if c != nil {
		c.Cancel()
	}
}

func (e *Engine) Paused() bool {
	e.controlMu.RLock()
	c := e.activeControl
	e.controlMu.RUnlock()
	return c != nil && c.Paused()
}

func (e *Engine) initMachines() {
	e.stepMachines = make([]*core.Machine, len(e.chain.Steps))
	e.blockMachines = make([][]*core.Machine, len(e.chain.Steps))
	for s := range e.chain.Steps {
		step := s + 1
		e.stepMachines[s] = core.NewMachine(core.StatePending, step, nil, "step")
		e.blockMachines[s] = make([]*core.Machine, len(e.grid))
		for b, rect := range e.grid {
			ref := checkpoint.BlockRef(b, rect)
			e.blockMachines[s][b] = core.NewMachine(core.StatePending, step, &ref, "block")
		}
	}
}

func (e *Engine) StepState(step int) core.State {
	e.stateMu.Lock()
	defer e.stateMu.Unlock()
	if step < 1 || step > len(e.stepMachines) {
		return ""
	}
	return e.stepMachines[step-1].State()
}

func (e *Engine) BlockState(step, blockIndex int) core.State {
	e.stateMu.Lock()
	defer e.stateMu.Unlock()
	if step < 1 || step > len(e.blockMachines) || blockIndex < 0 || blockIndex >= len(e.grid) {
		return ""
	}
	return e.blockMachines[step-1][blockIndex].State()
}

func (e *Engine) TryBlockTransition(step, blockIndex int, to core.State) error {
	e.stateMu.Lock()
	defer e.stateMu.Unlock()
	if step < 1 || step > len(e.blockMachines) || blockIndex < 0 || blockIndex >= len(e.grid) {
		return core.Range("block", "step/block index outside state matrix")
	}
	return e.blockMachines[step-1][blockIndex].Transition(to)
}

func (e *Engine) beginBlock(step, blockIndex int) error {
	e.stateMu.Lock()
	defer e.stateMu.Unlock()
	if step < 1 || step > len(e.blockMachines) {
		return core.Range("step", "step outside state matrix")
	}
	return e.blockMachines[step-1][blockIndex].Transition(core.StateProcessing)
}

func (e *Engine) finishBlock(step, blockIndex int, to core.State) error {
	e.stateMu.Lock()
	defer e.stateMu.Unlock()
	if step < 1 || step > len(e.blockMachines) {
		return core.Range("step", "step outside state matrix")
	}
	return e.blockMachines[step-1][blockIndex].Transition(to)
}

func (e *Engine) beginStep(step int) error {
	e.stateMu.Lock()
	defer e.stateMu.Unlock()
	m := e.stepMachines[step-1]
	switch m.State() {
	case core.StatePending:
		return m.Transition(core.StateProcessing)
	case core.StateProcessing:
		return nil
	default:
		return core.StateError(string(m.State()), string(core.StateProcessing), step, nil,
			"illegal step transition")
	}
}

func (e *Engine) finishStep(step int) error {
	e.stateMu.Lock()
	defer e.stateMu.Unlock()
	m := e.stepMachines[step-1]
	if m.State() == core.StateCommitted {
		return nil
	}
	return m.Transition(core.StateCommitted)
}

func (e *Engine) failStep(step int) {
	e.stateMu.Lock()
	defer e.stateMu.Unlock()
	m := e.stepMachines[step-1]
	if m.State() == core.StateProcessing {
		_ = m.Transition(core.StateFailed)
	}
}

func (e *Engine) meta(mode Mode) checkpoint.RunMeta {
	status := "running"
	return checkpoint.RunMeta{
		RunID: e.opts.RunID, ImageID: e.opts.ImageID,
		AlgorithmVersion:   e.opts.AlgorithmVersion,
		ChainFingerprint:   model.ChainFingerprint(e.chain, e.opts.AlgorithmVersion),
		ProjectFingerprint: e.opts.ProjectFingerprint,
		TruthFingerprint:   e.opts.TruthFingerprint,
		Width:              e.baseline.Width, Height: e.baseline.Height, Channels: e.baseline.Channels,
		BlockSize: e.opts.BlockSize, StepCount: len(e.chain.Steps), Status: status,
	}
}

func (e *Engine) Run(ctx context.Context, mode Mode) (*Result, error) {
	if e.ran {
		return nil, core.StateError("completed", "running", 0, nil, "engine already ran")
	}
	start := time.Now()
	e.opts.disableCache = mode == ModeFull
	control := NewControl(ctx)
	e.controlMu.Lock()
	e.activeControl = control
	if e.pauseRequested {
		control.Pause()
	}
	e.controlMu.Unlock()
	defer func() {
		e.controlMu.Lock()
		if e.activeControl == control {
			e.activeControl = nil
		}
		e.controlMu.Unlock()
	}()
	res := &Result{
		RunID: e.opts.RunID, ImageID: e.opts.ImageID, Mode: mode,
		BlockSize: e.opts.BlockSize, StepCount: len(e.chain.Steps),
		ChainFingerprint: model.ChainFingerprint(e.chain, e.opts.AlgorithmVersion),
	}
	meta := e.meta(mode)
	loaded := make(map[int]map[int]checkpoint.Artifact)
	fresh := false
	switch mode {
	case ModeLive:
		if err := e.store.EnsureRun(ctx, meta, true); err != nil {
			return nil, err
		}
		fresh = true
	case ModeFull:
		if err := e.store.EnsureRun(ctx, meta, true); err != nil {
			return nil, err
		}
		fresh = true
	case ModeResume:
		existing, ok, err := e.store.GetRunMeta(ctx, e.opts.RunID, e.opts.ImageID)
		if err != nil {
			return nil, err
		}
		if !ok {
			if err := e.store.EnsureRun(ctx, meta, false); err != nil {
				return nil, err
			}
			fresh = true
		} else {
			reason := e.metaMismatch(existing)
			if reason == "" {
				artifacts, err := e.store.LoadArtifacts(ctx, e.opts.RunID, e.opts.ImageID)
				if err != nil {
					return nil, err
				}
				if reason = e.loadAndValidate(artifacts, loaded); reason == "" {
					manifests, err := e.store.LoadBlockManifests(ctx, e.opts.RunID, e.opts.ImageID)
					if err != nil {
						return nil, err
					}
					reason = e.validateManifests(manifests, existing)
					if reason == "" {
						meta = existing
						res.Cache.LoadedReused = len(artifacts)
					}
				}
			}
			if reason != "" {
				res.Invalidated = true
				res.InvalidatedReason = reason
				res.FallbackFull = true
				if err := e.store.InvalidateRun(ctx, e.opts.RunID, e.opts.ImageID, reason); err != nil {
					return nil, err
				}
				if err := e.store.EnsureRun(ctx, meta, true); err != nil {
					return nil, err
				}
				loaded = make(map[int]map[int]checkpoint.Artifact)
				fresh = true
				mode = ModeFull
				e.opts.disableCache = true
				meta.Status = "running"
			}
		}
	default:
		return nil, core.Para(0, "mode", "unknown mode %q", mode)
	}
	if err := e.store.SetRunStatus(ctx, e.opts.RunID, e.opts.ImageID, "running"); err != nil {
		return nil, err
	}
	if fresh {
		if err := e.commitBaseline(ctx, res); err != nil {
			_ = e.store.SetRunStatus(ctx, e.opts.RunID, e.opts.ImageID, "failed")
			return nil, err
		}
	} else {
		if err := e.restoreBaselineKeys(res); err != nil {
			return nil, err
		}
		e.applyLoadedFrames(loaded)
		e.initLoadedStates(loaded)
	}
	pool := newWorkerPool(e, e.opts.Workers)
	defer pool.Close()
	remaining := e.opts.StopAfter
	total := len(e.chain.Steps) * len(e.grid)
	completed := 0
	interrupted := false
	for t := 1; t <= len(e.chain.Steps); t++ {
		if e.opts.StopAfter > 0 && remaining <= 0 {
			interrupted = true
			break
		}
		step := e.chain.Steps[t-1]
		rows := loaded[t]
		if len(rows) == len(e.grid) {
			completed += len(e.grid)
			continue
		}
		if e.frames[t-1] == nil {
			return e.failRun(ctx, res, core.Stage(t, core.BlockRef{Index: 0}, "previous step frame is incomplete"))
		}
		frame := e.frames[t-1].Clone()
		for b, a := range rows {
			if err := setFrameBlock(frame, e.grid[b], a.Pixels, e.baseline.Channels); err != nil {
				return e.failRun(ctx, res, core.Stage(t, checkpoint.BlockRef(b, e.grid[b]), "%v", err))
			}
			e.keys[t][b] = a.CacheKey
			e.inputHashes[t][b] = a.InputHash
		}
		missing := make([]int, 0, len(e.grid)-len(rows))
		for b := range e.grid {
			if _, ok := rows[b]; !ok {
				missing = append(missing, b)
			}
		}
		if remaining > 0 && len(missing) > remaining {
			missing = missing[:remaining]
			interrupted = true
		}
		tasks := make([]*blockTask, 0, len(missing))
		for _, b := range missing {
			rect := e.grid[b]
			upstream := e.keys[t-1][b]
			if upstream == "" {
				return e.failRun(ctx, res, core.Stage(t, checkpoint.BlockRef(b, rect), "missing upstream cache key"))
			}
			inputRegion := rect
			radius := model.InputSupportRadius(step)
			if radius > 0 {
				inputRegion = rect.ExpandClamped(radius, e.baseline.Width, e.baseline.Height)
			}
			inputHash := core.HashFrameRegion(e.frames[t-1], inputRegion)
			key := CacheKey(e.opts.AlgorithmVersion, t, inputHash, step.CanonicalParams(), upstream)
			tasks = append(tasks, &blockTask{
				order: len(tasks), step: t, blockIndex: b, rect: rect,
				prev: e.frames[t-1], action: step.RegionRect(e.baseline.Width, e.baseline.Height),
				inputRegion: inputRegion, inputHash: inputHash, cacheKey: key,
				upstreamKey: upstream, queuedAt: time.Now(),
				ctx: control.Context(), control: control,
			})
		}
		if len(tasks) == 0 {
			if interrupted {
				break
			}
			return e.failRun(ctx, res, core.Stage(t, core.BlockRef{Index: 0}, "no tasks available for incomplete step"))
		}
		if err := e.beginStep(t); err != nil {
			return e.failRun(ctx, res, err)
		}
		results, queueWaits, err := e.runTasks(control, pool, tasks)
		res.QueueWaits = append(res.QueueWaits, queueWaits...)
		if err != nil {
			if core.CodeOf(err) == core.CodeCanceled {
				_ = e.store.SetRunStatus(ctx, e.opts.RunID, e.opts.ImageID, "interrupted")
				res.Interrupted = true
				res.Status = "interrupted"
				return res, err
			}
			return e.failRun(ctx, res, err)
		}
		for _, tr := range results {
			res.BufferWaits = append(res.BufferWaits, tr.bufferWait...)
			if tr.cacheHit {
				res.Cache.Hits++
				if tr.persistent {
					res.Cache.PersistentHits++
				}
			} else {
				res.Cache.Misses++
			}
		}
		sort.Slice(results, func(i, j int) bool { return results[i].task.order < results[j].task.order })
		for _, tr := range results {
			if err := control.Wait(ctx); err != nil {
				interrupted = true
				res.Interrupted = true
				break
			}
			a := checkpoint.Artifact{
				RunID: e.opts.RunID, ImageID: e.opts.ImageID, Step: t,
				Block: tr.task.rect, State: core.StateCommitted,
				CacheKey: tr.task.cacheKey, InputHash: tr.task.inputHash,
				OutputHash:       checkpoint.PixelHash(tr.pixels),
				ChainFingerprint: res.ChainFingerprint,
				AlgorithmVersion: e.opts.AlgorithmVersion,
				Pixels:           tr.pixels,
			}
			a.OutputDigest = a.ComputeOutputDigest()
			committed, err := e.store.CommitArtifact(ctx, a)
			if err != nil {
				_ = e.finishBlock(t, tr.task.blockIndex, core.StateFailed)
				return e.failRun(ctx, res, core.Stage(t, checkpoint.BlockRef(tr.task.blockIndex, tr.task.rect),
					"checkpoint commit failed: %v", err))
			}
			if err := e.finishBlock(t, tr.task.blockIndex, core.StateCommitted); err != nil {
				return e.failRun(ctx, res, err)
			}
			if err := setFrameBlock(frame, tr.task.rect, tr.pixels, e.baseline.Channels); err != nil {
				return e.failRun(ctx, res, core.Stage(t, checkpoint.BlockRef(tr.task.blockIndex, tr.task.rect), "%v", err))
			}
			e.keys[t][tr.task.blockIndex] = tr.task.cacheKey
			e.inputHashes[t][tr.task.blockIndex] = tr.task.inputHash
			if loaded[t] == nil {
				loaded[t] = make(map[int]checkpoint.Artifact)
			}
			loaded[t][tr.task.blockIndex] = committed
			completed++
			remaining--
			if e.opts.OnProgress != nil {
				e.opts.OnProgress(completed, total)
			}
		}
		if interrupted || len(loaded[t]) < len(e.grid) {
			interrupted = true
			break
		}
		e.frames[t] = frame
		if err := e.finishStep(t); err != nil {
			return e.failRun(ctx, res, err)
		}
		if e.opts.OnStepComplete != nil {
			e.opts.OnStepComplete(t, frame.Clone())
		}
	}
	res.StepStates = e.stepStates()
	res.StepFrames = e.frames
	res.Interrupted = interrupted
	if interrupted {
		res.Status = "interrupted"
		_ = e.store.SetRunStatus(ctx, e.opts.RunID, e.opts.ImageID, "interrupted")
		res.Elapsed = time.Since(start)
		e.ran = true
		return res, nil
	}
	if err := e.evaluate(); err != nil {
		return e.failRun(ctx, res, err)
	}
	if err := e.commitManifests(ctx); err != nil {
		return e.failRun(ctx, res, err)
	}
	res.Completed = true
	res.Status = "completed"
	res.Evaluations = e.evals
	res.StepTraces = e.traces
	res.StepStates = e.stepStates()
	res.Elapsed = time.Since(start)
	if err := e.store.SetRunStatus(ctx, e.opts.RunID, e.opts.ImageID, "completed"); err != nil {
		return nil, err
	}
	e.revision++
	res.VersionID = fmt.Sprintf("v%d", e.revision)
	e.versions = append(e.versions, Version{
		ID: res.VersionID, Chain: e.chain.Clone(), Frames: e.frames,
		Keys: cloneKeys(e.keys), InputHashes: cloneKeys(e.inputHashes),
		Evaluations: e.evals, Cache: res.Cache,
	})
	// Full recomputation intentionally bypasses cache reads, but subsequent
	// edits and rollbacks on this engine must be able to reuse prior entries.
	e.opts.disableCache = false
	e.ran = true
	return res, nil
}

func (e *Engine) validateManifests(manifests []checkpoint.BlockManifest, meta checkpoint.RunMeta) string {
	if len(manifests) == 0 {
		if meta.Status == "completed" {
			return "completed run has no block manifests"
		}
		return ""
	}
	if meta.Status == "completed" && len(manifests) != len(e.grid) {
		return "block manifests are incomplete"
	}
	chainFP := model.ChainFingerprint(e.chain, e.opts.AlgorithmVersion)
	seen := make(map[string]bool, len(manifests))
	for _, m := range manifests {
		if m.AlgorithmVersion != e.opts.AlgorithmVersion || m.ChainFingerprint != chainFP {
			return "block manifest version or chain fingerprint mismatch"
		}
		key := fmt.Sprintf("%d,%d", m.Block.X, m.Block.Y)
		if seen[key] {
			return "duplicate block manifest"
		}
		seen[key] = true
		if _, ok := e.index[key]; !ok || m.Block != e.grid[e.index[key]] {
			return "block manifest is outside configured grid"
		}
		if m.ContentHash != m.ComputeContentHash() {
			return "block manifest content hash mismatch"
		}
		if len(m.Steps) != len(e.chain.Steps)+1 {
			return "block manifest step count mismatch"
		}
		for i, step := range m.Steps {
			if step.Step != i || step.CacheKey == "" || step.OutputHash == "" || step.OutputDigest == "" {
				return "block manifest step summary is malformed"
			}
		}
	}
	return ""
}

func (e *Engine) failRun(ctx context.Context, res *Result, err error) (*Result, error) {
	_ = e.store.SetRunStatus(ctx, e.opts.RunID, e.opts.ImageID, "failed")
	res.Status = "failed"
	res.StepStates = e.stepStates()
	res.StepFrames = e.frames
	e.ran = true
	return res, err
}

func (e *Engine) runTasks(control *Control, pool *workerPool, tasks []*blockTask) ([]taskResult, []WaitEvent, error) {
	if len(tasks) == 0 {
		return nil, nil, nil
	}
	resCh := make(chan taskResult, len(tasks))
	for _, t := range tasks {
		t.resultCh = resCh
	}
	var queueWaits []WaitEvent
	var qmu sync.Mutex
	dispatchDone := make(chan struct{})
	go func() {
		defer close(dispatchDone)
		for _, t := range tasks {
			start := time.Now()
			select {
			case pool.tasks <- t:
				if d := time.Since(start); d >= 50*time.Microsecond {
					qmu.Lock()
					queueWaits = append(queueWaits, WaitEvent{
						Step: t.step, Block: t.blockIndex,
						Reason: "worker queue full", Duration: d,
					})
					qmu.Unlock()
				}
			case <-control.Context().Done():
				t.resultCh <- taskResult{task: t, err: canceledError()}
			}
		}
	}()
	results := make([]taskResult, 0, len(tasks))
	var firstErr error
	for range tasks {
		r := <-resCh
		if r.err != nil && firstErr == nil {
			firstErr = r.err
		}
		results = append(results, r)
	}
	<-dispatchDone
	qmu.Lock()
	qw := append([]WaitEvent(nil), queueWaits...)
	qmu.Unlock()
	if firstErr != nil {
		return results, qw, firstErr
	}
	return results, qw, nil
}

func (e *Engine) executeTask(task *blockTask) (out taskResult) {
	out.task = task
	defer func() {
		if r := recover(); r != nil {
			out.err = core.Stage(task.step, checkpoint.BlockRef(task.blockIndex, task.rect),
				"panic in stage: %v\n%s", r, debug.Stack())
		}
	}()
	out.queueWait = time.Since(task.queuedAt)
	if err := task.control.Wait(task.ctx); err != nil {
		out.err = err
		return out
	}
	if err := e.beginBlock(task.step, task.blockIndex); err != nil {
		out.err = err
		return out
	}
	opts := e.opts
	if opts.Fault != nil {
		if err := opts.Fault(task.step, task.rect); err != nil {
			out.err = core.Stage(task.step, checkpoint.BlockRef(task.blockIndex, task.rect),
				"injected stage failure: %v", err)
			_ = e.finishBlock(task.step, task.blockIndex, core.StateFailed)
			return out
		}
	}

	if e.opts.ModeUsesCache() {
		if v, ok := e.cache.Get(task.cacheKey); ok && len(v.Pixels) == task.rect.W*task.rect.H*e.baseline.Channels {
			out.pixels = append([]byte(nil), v.Pixels...)
			out.cacheHit = true
			if len(out.pixels) > 0 {
				return out
			}
		}
	}
	if e.opts.ModeUsesCache() && e.opts.PersistentCache {
		if a, ok, err := e.store.LookupCache(task.ctx, task.cacheKey, e.opts.AlgorithmVersion); err == nil && ok {
			if len(a.Pixels) == task.rect.W*task.rect.H*e.baseline.Channels {
				e.cache.Put(task.cacheKey, BlockValue{Pixels: a.Pixels, CacheKey: a.CacheKey,
					InputHash: a.InputHash, OutputHash: a.OutputHash})
				out.pixels = a.Pixels
				out.cacheHit = true
				out.persistent = true
				return out
			}
		}
	}

	required := fullFrameScratch(e.baseline)
	release, waits, err := e.buffers.Acquire(task.ctx, required, task.step, task.blockIndex,
		"pixel buffer exhausted")
	if err != nil {
		out.err = core.Stage(task.step, checkpoint.BlockRef(task.blockIndex, task.rect),
			"pixel buffer unavailable: %v", err)
		_ = e.finishBlock(task.step, task.blockIndex, core.StateFailed)
		return out
	}
	defer release()
	out.bufferWait = waits
	if err := task.control.Wait(task.ctx); err != nil {
		_ = e.finishBlock(task.step, task.blockIndex, core.StateFailed)
		out.err = err
		return out
	}
	if e.opts.TaskDelay > 0 {
		timer := time.NewTimer(e.opts.TaskDelay)
		select {
		case <-timer.C:
		case <-task.ctx.Done():
			timer.Stop()
			_ = e.finishBlock(task.step, task.blockIndex, core.StateFailed)
			out.err = canceledError()
			return out
		}
	}
	blockFrame, err := ops.ApplyBlock(task.prev, e.chain.Steps[task.step-1], task.rect, ops.Context{
		Seed: e.opts.ImageSeed, StepIndex: task.step - 1,
	})
	if err != nil {
		out.err = core.Stage(task.step, checkpoint.BlockRef(task.blockIndex, task.rect), "%v", err)
		_ = e.finishBlock(task.step, task.blockIndex, core.StateFailed)
		return out
	}
	out.pixels = append([]byte(nil), blockFrame.Pix...)
	if e.opts.ModeUsesCache() {
		e.cache.Put(task.cacheKey, BlockValue{
			Pixels: out.pixels, CacheKey: task.cacheKey, InputHash: task.inputHash,
			OutputHash: checkpoint.PixelHash(out.pixels),
		})
	}
	return out
}

func (o Options) ModeUsesCache() bool {
	return o.RunID != "" && !o.disableCache
}

// disableCache is kept unexported so callers select full recomputation via ModeFull.
func (o *Options) SetFullRecompute() { o.disableCache = true }

func (e *Engine) commitBaseline(ctx context.Context, res *Result) error {
	e.frames[0] = e.baseline
	for b, rect := range e.grid {
		pixels := e.baseline.BlockBytes(rect)
		inputHash := checkpoint.PixelHash(pixels)
		key := RootCacheKey(inputHash)
		a := checkpoint.Artifact{
			RunID: e.opts.RunID, ImageID: e.opts.ImageID, Step: 0, Block: rect,
			State: core.StateCommitted, CacheKey: key, InputHash: inputHash,
			OutputHash: inputHash, ChainFingerprint: model.ChainFingerprint(e.chain, e.opts.AlgorithmVersion),
			AlgorithmVersion: e.opts.AlgorithmVersion, Pixels: pixels,
		}
		a.OutputDigest = a.ComputeOutputDigest()
		committed, err := e.store.CommitArtifact(ctx, a)
		if err != nil {
			return core.Stage(0, checkpoint.BlockRef(b, rect), "baseline checkpoint failed: %v", err)
		}
		e.keys[0][b] = key
		e.inputHashes[0][b] = inputHash
		res.Cache.LoadedReused += 0
		_ = committed
	}
	res.Cache.LoadedReused = 0
	return nil
}

func (e *Engine) restoreBaselineKeys(res *Result) error {
	for b, rect := range e.grid {
		pixels := e.baseline.BlockBytes(rect)
		inputHash := checkpoint.PixelHash(pixels)
		e.keys[0][b] = RootCacheKey(inputHash)
		e.inputHashes[0][b] = inputHash
	}
	return nil
}

func (e *Engine) metaMismatch(m checkpoint.RunMeta) string {
	if m.AlgorithmVersion != e.opts.AlgorithmVersion {
		return "algorithm version mismatch"
	}
	if m.ChainFingerprint != model.ChainFingerprint(e.chain, e.opts.AlgorithmVersion) {
		return "chain fingerprint mismatch"
	}
	if m.ProjectFingerprint != e.opts.ProjectFingerprint {
		return "project fingerprint mismatch"
	}
	if m.TruthFingerprint != e.opts.TruthFingerprint {
		return "truth fingerprint mismatch"
	}
	if m.Width != e.baseline.Width || m.Height != e.baseline.Height || m.Channels != e.baseline.Channels {
		return "frame geometry mismatch"
	}
	if m.BlockSize != e.opts.BlockSize || m.StepCount != len(e.chain.Steps) {
		return "block size or step count mismatch"
	}
	return ""
}

func (e *Engine) loadAndValidate(artifacts []checkpoint.Artifact, loaded map[int]map[int]checkpoint.Artifact) string {
	seenBaseline := make(map[int]bool)
	chainFP := model.ChainFingerprint(e.chain, e.opts.AlgorithmVersion)
	for _, a := range artifacts {
		if a.AlgorithmVersion != e.opts.AlgorithmVersion {
			return "artifact algorithm version mismatch"
		}
		if a.ChainFingerprint != chainFP {
			return "artifact chain fingerprint mismatch"
		}
		if a.State != core.StateCommitted {
			return "artifact is not committed"
		}
		if err := a.Validate(); err != nil {
			return "artifact digest validation failed: " + err.Error()
		}
		if checkpoint.PixelHash(a.Pixels) != a.OutputHash {
			return "artifact pixel content hash mismatch"
		}
		if a.Step < 0 || a.Step > len(e.chain.Steps) {
			return "artifact step outside chain"
		}
		b, ok := e.index[fmt.Sprintf("%d,%d", a.Block.X, a.Block.Y)]
		if !ok || a.Block != e.grid[b] {
			return "artifact block is outside configured grid"
		}
		want := a.Block.W * a.Block.H * e.baseline.Channels
		if len(a.Pixels) != want {
			return "artifact pixel byte count mismatch"
		}
		if a.Step == 0 {
			basePixels := e.baseline.BlockBytes(a.Block)
			if checkpoint.PixelHash(basePixels) != a.OutputHash {
				return "baseline block content differs"
			}
			seenBaseline[b] = true
			continue
		}
		if loaded[a.Step] == nil {
			loaded[a.Step] = make(map[int]checkpoint.Artifact)
		}
		if _, dup := loaded[a.Step][b]; dup {
			return "duplicate artifact block"
		}
		loaded[a.Step][b] = a
	}
	if len(seenBaseline) != len(e.grid) {
		return "baseline checkpoint is incomplete"
	}
	for t := 2; t <= len(e.chain.Steps); t++ {
		if len(loaded[t]) > 0 && len(loaded[t-1]) != len(e.grid) {
			return "non-contiguous step checkpoint"
		}
	}
	return ""
}

func (e *Engine) applyLoadedFrames(loaded map[int]map[int]checkpoint.Artifact) {
	for t := 1; t <= len(e.chain.Steps); t++ {
		rows := loaded[t]
		if len(rows) != len(e.grid) {
			break
		}
		frame := e.frames[t-1].Clone()
		for b, a := range rows {
			_ = setFrameBlock(frame, e.grid[b], a.Pixels, e.baseline.Channels)
			e.keys[t][b] = a.CacheKey
			e.inputHashes[t][b] = a.InputHash
		}
		e.frames[t] = frame
	}
}

func (e *Engine) initLoadedStates(loaded map[int]map[int]checkpoint.Artifact) {
	e.stateMu.Lock()
	defer e.stateMu.Unlock()
	for t := 1; t <= len(e.chain.Steps); t++ {
		count := len(loaded[t])
		stepState := core.StatePending
		if count == len(e.grid) {
			stepState = core.StateCommitted
		} else if count > 0 {
			stepState = core.StateProcessing
		}
		e.stepMachines[t-1] = core.NewMachine(stepState, t, nil, "step")
		for b, rect := range e.grid {
			state := core.StatePending
			if _, ok := loaded[t][b]; ok {
				state = core.StateCommitted
			}
			ref := checkpoint.BlockRef(b, rect)
			e.blockMachines[t-1][b] = core.NewMachine(state, t, &ref, "block")
		}
	}
}

func (e *Engine) evaluate() error {
	evals := make([]quality.BlockEvaluation, len(e.grid))
	for b, rect := range e.grid {
		if e.frames[len(e.chain.Steps)] == nil {
			return core.Stage(len(e.chain.Steps), checkpoint.BlockRef(b, rect), "final frame is incomplete")
		}
		evals[b] = quality.EvaluateBlock(e.baseline, e.frames[1:], rect,
			e.optsAttributionShare(), e.chain)
	}
	e.evals = evals
	traces := make([]StepTrace, len(e.chain.Steps))
	for i, step := range e.chain.Steps {
		t := i + 1
		action := step.RegionRect(e.baseline.Width, e.baseline.Height)
		input := action.ExpandClamped(model.InputSupportRadius(step), e.baseline.Width, e.baseline.Height)
		traces[i] = StepTrace{
			Step: t, Type: string(step.Type), InputRegion: input, ActionRegion: action,
			OutputRegion: action, InputDigest: e.frames[t-1].Hash(), OutputDigest: e.frames[t].Hash(),
			SupportRadius: model.InputSupportRadius(step), BlockCount: len(e.grid),
		}
	}
	e.traces = traces
	return nil
}

func (e *Engine) commitManifests(ctx context.Context) error {
	chainFP := model.ChainFingerprint(e.chain, e.opts.AlgorithmVersion)
	for b, rect := range e.grid {
		steps := make([]checkpoint.StepSummary, 0, len(e.chain.Steps)+1)
		for t := 0; t <= len(e.chain.Steps); t++ {
			pixels := e.frames[t].BlockBytes(rect)
			outputHash := checkpoint.PixelHash(pixels)
			a := checkpoint.Artifact{
				RunID: e.opts.RunID, ImageID: e.opts.ImageID, Step: t, Block: rect,
				CacheKey: e.keys[t][b], InputHash: e.inputHashes[t][b],
				OutputHash: outputHash, ChainFingerprint: chainFP,
				AlgorithmVersion: e.opts.AlgorithmVersion, Pixels: pixels,
			}
			steps = append(steps, checkpoint.StepSummary{
				Step: t, CacheKey: a.CacheKey, InputHash: a.InputHash,
				OutputHash: a.OutputHash, OutputDigest: a.ComputeOutputDigest(),
			})
		}
		if _, err := e.store.CommitBlockManifest(ctx, checkpoint.BlockManifest{
			RunID: e.opts.RunID, ImageID: e.opts.ImageID, Block: rect,
			ChainFingerprint: chainFP, AlgorithmVersion: e.opts.AlgorithmVersion,
			Steps: steps,
		}); err != nil {
			return core.Stage(0, checkpoint.BlockRef(b, rect), "block manifest commit failed: %v", err)
		}
	}
	return nil
}

func (e *Engine) optsAttributionShare() float64 {
	return e.opts.AttributionShare
}

func (e *Engine) stepStates() []core.State {
	e.stateMu.Lock()
	defer e.stateMu.Unlock()
	out := make([]core.State, len(e.stepMachines))
	for i, m := range e.stepMachines {
		out[i] = m.State()
	}
	return out
}

func setFrameBlock(frame *core.Frame, rect core.Rect, pixels []byte, channels int) error {
	bf, err := core.NewFrame(rect.W, rect.H, channels)
	if err != nil {
		return err
	}
	if len(pixels) != len(bf.Pix) {
		return fmt.Errorf("block pixel length %d != %d", len(pixels), len(bf.Pix))
	}
	copy(bf.Pix, pixels)
	return frame.SetBlock(rect, bf)
}

func cloneKeys(in [][]string) [][]string {
	out := make([][]string, len(in))
	for i := range in {
		out[i] = append([]string(nil), in[i]...)
	}
	return out
}

func (e *Engine) MarshalState() ([]byte, error) {
	e.stateMu.Lock()
	defer e.stateMu.Unlock()
	stepStates := make([]core.State, len(e.stepMachines))
	for i, m := range e.stepMachines {
		stepStates[i] = m.State()
	}
	type blockStateRow struct {
		Step   int          `json:"step"`
		States []core.State `json:"states"`
	}
	blockStates := make([]blockStateRow, len(e.blockMachines))
	for i, row := range e.blockMachines {
		blockStates[i].Step = i + 1
		blockStates[i].States = make([]core.State, len(row))
		for j, m := range row {
			blockStates[i].States[j] = m.State()
		}
	}
	return json.Marshal(map[string]any{
		"step_states":  stepStates,
		"revision":     e.revision,
		"chain":        e.chain,
		"block_states": blockStates,
	})
}

func max(a, b int) int {
	if a > b {
		return a
	}
	return b
}

func fullFrameScratch(f *core.Frame) int64 {
	if f == nil {
		return 0
	}
	return int64(f.Width*f.Height*f.Channels*2 + 4096)
}
