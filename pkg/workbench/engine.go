package workbench

import (
	"context"
	"fmt"
	"math"
	"sync"
	"time"
)

type LogEntry struct {
	At      string `json:"at"`
	Level   string `json:"level"`
	Segment int    `json:"segment,omitempty"`
	Message string `json:"message"`
}

type EngineStatus struct {
	SceneID          string           `json:"sceneId"`
	Running          bool             `json:"running"`
	Paused           bool             `json:"paused"`
	Closed           bool             `json:"closed"`
	Progress         Progress         `json:"progress"`
	ParamVersion     string           `json:"paramVersion"`
	Config           ProcessConfig    `json:"config"`
	Scale            ScaleDecision    `json:"scale"`
	Segments         []Segment        `json:"segments"`
	Logs             []LogEntry       `json:"logs"`
	PeakRadiance     float64          `json:"peakRadiance"`
	OutOfGamut       int              `json:"outOfGamut"`
	ActiveWorkers    int              `json:"activeWorkers"`
	MaxWorkers       int              `json:"maxWorkers"`
	BufferCapacity   int              `json:"bufferCapacity"`
	BuffersInUse     int              `json:"buffersInUse"`
	Checkpoint       CheckpointReport `json:"checkpoint"`
	ResultReady      bool             `json:"resultReady"`
	ContentVersion   string           `json:"contentVersion"`
	FirstFailureText string           `json:"firstFailure,omitempty"`
}

type Engine struct {
	mu            sync.Mutex
	cond          *sync.Cond
	scene         *Scene
	cfg           ProcessConfig
	paramVersion  string
	scale         ScaleDecision
	segments      []*Segment
	store         *CheckpointStore
	checkpoint    CheckpointReport
	paused        bool
	running       bool
	closed        bool
	active        int
	slots         chan struct{}
	buffers       chan struct{}
	logs          []LogEntry
	faults        map[int]string
	startedAt     time.Time
	contentSerial uint64
	paramSerial   int
}

func NewEngine(scene *Scene, cfg ProcessConfig, store *CheckpointStore) (*Engine, error) {
	if scene == nil {
		return nil, fieldErr(CodeMeta, "scene", nil, "non-nil scene", "scene is missing")
	}
	if err := ValidateScene(scene); err != nil {
		return nil, err
	}
	cfg = cfg.normalized()
	if err := cfg.Validate(); err != nil {
		return nil, err
	}
	scale := DecideScale(scene.Meta, cfg)
	populateCoarseStats(scene, &scale, cfg)
	cfg.Path = scale.SelectedPath
	segments := buildSegments(scene, scale)
	e := &Engine{
		scene:        scene,
		cfg:          cfg,
		paramVersion: "p1",
		paramSerial:  1,
		scale:        scale,
		segments:     segments,
		store:        store,
		faults:       make(map[int]string),
		startedAt:    time.Now(),
	}
	e.cond = sync.NewCond(&e.mu)
	e.slots = make(chan struct{}, cfg.MaxWorkers)
	e.buffers = make(chan struct{}, cfg.MaxWorkers)
	e.addLogLocked("info", -1, fmt.Sprintf("engine created: %d segments, path=%s, tile=%dx%d, downsample=x%d", len(segments), scale.SelectedPath, scale.TileWidth, scale.TileHeight, scale.DownsampleFactor))
	if store != nil {
		loaded, report := store.Load(scene)
		e.checkpoint = report
		if report.Invalidated {
			e.addLogLocked("warn", -1, "snapshot invalidated: "+report.Reason+"; scheduling full recompute")
		}
		for index, snap := range loaded {
			if index < 0 || index >= len(e.segments) {
				continue
			}
			seg := e.segments[index]
			if snap.X != seg.X || snap.Y != seg.Y || snap.Width != seg.Width || snap.Height != seg.Height {
				continue
			}
			seg.Snapshot = snap
			seg.State = StateCommitted
			seg.ParamVersion = snap.Params.Version
			seg.DownsampleFactor = snap.DownsampleFactor
			seg.Peak = snap.Peak
			seg.ContentHash = snap.ContentHash
			seg.CommittedAt = time.Now().UTC().Format(time.RFC3339Nano)
		}
		if report.Loaded > 0 {
			e.addLogLocked("info", -1, fmt.Sprintf("restored %d committed segments from checkpoint", report.Loaded))
		}
	}
	return e, nil
}

func buildSegments(scene *Scene, scale ScaleDecision) []*Segment {
	tw := maxInt(1, scale.TileWidth)
	th := maxInt(1, scale.TileHeight)
	var segments []*Segment
	index := 0
	for y := 0; y < scene.Meta.Height; y += th {
		for x := 0; x < scene.Meta.Width; x += tw {
			rect := roundRect(x, y, tw, th, scene.Meta.Width, scene.Meta.Height)
			segments = append(segments, &Segment{
				Index:  index,
				X:      rect.X,
				Y:      rect.Y,
				Width:  rect.W,
				Height: rect.H,
				State:  StatePending,
			})
			index++
		}
	}
	return segments
}

func (e *Engine) addLogLocked(level string, segment int, message string) {
	e.logs = append(e.logs, LogEntry{
		At: time.Now().UTC().Format(time.RFC3339Nano), Level: level, Segment: segment, Message: message,
	})
	if len(e.logs) > 300 {
		e.logs = append([]LogEntry(nil), e.logs[len(e.logs)-300:]...)
	}
}

func (e *Engine) Log(level string, segment int, message string) {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.addLogLocked(level, segment, message)
}

func (e *Engine) Start() error {
	e.mu.Lock()
	if e.closed {
		e.mu.Unlock()
		return wbErr(CodeState, "engine is closed")
	}
	if e.running {
		e.paused = false
		e.cond.Broadcast()
		e.mu.Unlock()
		return nil
	}
	e.running = true
	e.paused = false
	e.addLogLocked("info", -1, "processing started")
	e.cond.Broadcast()
	e.mu.Unlock()
	go e.scheduler()
	return nil
}

func (e *Engine) Pause() error {
	e.mu.Lock()
	defer e.mu.Unlock()
	if e.closed {
		return wbErr(CodeState, "engine is closed")
	}
	e.paused = true
	e.addLogLocked("info", -1, "pause requested; in-flight segments will finish")
	e.cond.Broadcast()
	return nil
}

func (e *Engine) Resume() error {
	e.mu.Lock()
	if e.closed {
		e.mu.Unlock()
		return wbErr(CodeState, "engine is closed")
	}
	e.paused = false
	e.addLogLocked("info", -1, "resume requested")
	if e.running {
		e.cond.Broadcast()
		e.mu.Unlock()
		return nil
	}
	e.running = true
	e.cond.Broadcast()
	e.mu.Unlock()
	go e.scheduler()
	return nil
}

func (e *Engine) scheduler() {
	for {
		e.mu.Lock()
		for e.paused && !e.closed {
			e.cond.Wait()
		}
		if e.closed {
			e.running = false
			e.cond.Broadcast()
			e.mu.Unlock()
			return
		}
		index := -1
		for _, seg := range e.segments {
			if seg.State == StatePending && !seg.Dispatch {
				index = seg.Index
				break
			}
		}
		if index < 0 {
			if e.active == 0 {
				e.running = false
				e.cond.Broadcast()
				e.mu.Unlock()
				return
			}
			e.cond.Wait()
			e.mu.Unlock()
			continue
		}
		seg := e.segments[index]
		seg.Dispatch = true
		cfg := e.cfg
		version := e.paramVersion
		scale := e.scale
		e.mu.Unlock()

		select {
		case e.slots <- struct{}{}:
		default:
			e.Log("queue", index, "all per-thread pixel buffers are in use; segment queued")
			e.slots <- struct{}{}
		}
		select {
		case e.buffers <- struct{}{}:
		default:
			e.Log("queue", index, "per-thread pixel buffer exhausted; segment queued until a buffer is released")
			e.buffers <- struct{}{}
		}

		e.mu.Lock()
		seg.Dispatch = false
		if e.closed {
			e.running = false
			e.cond.Broadcast()
			e.mu.Unlock()
			<-e.buffers
			<-e.slots
			return
		}
		if err := transitionLocked(seg, StateProcessing); err != nil {
			e.addLogLocked("error", index, err.Error())
			e.mu.Unlock()
			<-e.buffers
			<-e.slots
			continue
		}
		seg.ParamVersion = version
		seg.DownsampleFactor = scale.DownsampleFactor
		seg.StartedAt = time.Now().UTC().Format(time.RFC3339Nano)
		seg.Error = nil
		e.active++
		e.addLogLocked("info", index, fmt.Sprintf("segment started with %s params", version))
		e.cond.Broadcast()
		e.mu.Unlock()
		go e.runJob(seg, cfg, version, scale)
	}
}

func (e *Engine) runJob(seg *Segment, cfg ProcessConfig, version string, scale ScaleDecision) {
	index := seg.Index
	defer func() {
		<-e.buffers
		<-e.slots
		e.mu.Lock()
		e.active--
		e.contentSerial++
		e.cond.Broadcast()
		e.mu.Unlock()
	}()
	defer func() {
		if r := recover(); r != nil {
			e.failSegment(index, wbErr(CodeInternal, "segment panic: %v", r))
		}
	}()
	if err := e.executeSegment(seg, cfg, version, scale); err != nil {
		e.failSegment(index, err)
	}
}

func (e *Engine) executeSegment(seg *Segment, cfg ProcessConfig, version string, scale ScaleDecision) error {
	e.mu.Lock()
	if fault, ok := e.faults[seg.Index]; ok {
		e.mu.Unlock()
		return fieldErr(CodeInternal, "faultInjection", fault, "none", "injected segment failure")
	}
	scene := e.scene
	e.mu.Unlock()
	ctx, err := newRenderContext(scene, cfg, scale)
	if err != nil {
		return err
	}
	rect := blockRect{X: seg.X, Y: seg.Y, W: seg.Width, H: seg.Height}
	block, err := ctx.renderBlock(rect)
	if err != nil {
		return err
	}
	params := snapshotParams(cfg, version)
	snap := &SegmentSnapshot{
		Index:            seg.Index,
		X:                seg.X,
		Y:                seg.Y,
		Width:            seg.Width,
		Height:           seg.Height,
		Fused:            block.Fused,
		Chroma:           block.LinearRGB,
		Display:          block.Display,
		GamutMask:        block.GamutMask,
		Peak:             block.Peak,
		Mean:             block.Mean,
		OutOfGamut:       block.OutOfGamut,
		OutOfGamutSample: block.OOFSamples,
		Params:           params,
		BandMetaVersion:  scene.Meta.BandMetadataVersion,
		AlgorithmVersion: AlgorithmVersion,
		DownsampleFactor: scale.DownsampleFactor,
		Path:             scale.SelectedPath,
	}
	snap.ContentHash = segmentHash(snap)
	if e.store != nil {
		if err := e.store.Save(scene, snap); err != nil {
			return err
		}
	}
	e.mu.Lock()
	defer e.mu.Unlock()
	if seg.State != StateProcessing {
		return illegalTransition(seg.Index, seg.State, StateCommitted)
	}
	seg.Snapshot = snap
	seg.Peak = snap.Peak
	seg.ContentHash = snap.ContentHash
	seg.ParamVersion = version
	seg.DownsampleFactor = snap.DownsampleFactor
	seg.CommittedAt = time.Now().UTC().Format(time.RFC3339Nano)
	seg.Error = nil
	if err := transitionLocked(seg, StateCommitted); err != nil {
		return err
	}
	e.checkpoint.Loaded = e.countCommittedLocked()
	e.addLogLocked("info", seg.Index, fmt.Sprintf("segment committed hash=%s", shortHash(snap.ContentHash)))
	e.cond.Broadcast()
	return nil
}

func (e *Engine) failSegment(index int, err error) {
	we := AsWorkbenchError(err)
	e.mu.Lock()
	defer e.mu.Unlock()
	if index < 0 || index >= len(e.segments) {
		return
	}
	seg := e.segments[index]
	if seg.State != StateProcessing {
		e.addLogLocked("error", index, "failure arrived outside processing state: "+we.Error())
		return
	}
	seg.Error = we
	if transitionErr := transitionLocked(seg, StateFailed); transitionErr != nil {
		e.addLogLocked("error", index, "failed to mark segment failed: "+transitionErr.Error())
		return
	}
	e.addLogLocked("error", index, "segment failed: "+we.Error())
	e.cond.Broadcast()
}

func (e *Engine) Step() error {
	e.mu.Lock()
	if e.closed {
		e.mu.Unlock()
		return wbErr(CodeState, "engine is closed")
	}
	if e.running || e.active > 0 {
		e.mu.Unlock()
		return fieldErr(CodeState, "running", true, false, "step requires an idle engine")
	}
	index := -1
	for _, seg := range e.segments {
		if seg.State == StatePending {
			index = seg.Index
			break
		}
	}
	if index < 0 {
		e.mu.Unlock()
		return wbErr(CodeState, "no pending segment is available for step")
	}
	seg := e.segments[index]
	if err := transitionLocked(seg, StateProcessing); err != nil {
		e.mu.Unlock()
		return err
	}
	seg.ParamVersion = e.paramVersion
	seg.DownsampleFactor = e.scale.DownsampleFactor
	seg.StartedAt = time.Now().UTC().Format(time.RFC3339Nano)
	cfg := e.cfg
	version := e.paramVersion
	scale := e.scale
	e.addLogLocked("info", index, "single segment step started")
	e.mu.Unlock()
	if err := e.executeSegment(seg, cfg, version, scale); err != nil {
		e.failSegment(index, err)
		return err
	}
	return nil
}

func (e *Engine) RetrySegment(index int) error {
	e.mu.Lock()
	defer e.mu.Unlock()
	if index < 0 || index >= len(e.segments) {
		return fieldErr(CodeState, "segment", index, "valid segment index", "segment index is out of range")
	}
	seg := e.segments[index]
	if err := transitionLocked(seg, StatePending); err != nil {
		return err
	}
	seg.Error = nil
	seg.StartedAt = ""
	seg.CommittedAt = ""
	e.addLogLocked("warn", index, "failed segment requeued for retry")
	e.cond.Broadcast()
	return nil
}

func (e *Engine) InjectFault(index int, message string) error {
	e.mu.Lock()
	defer e.mu.Unlock()
	if index < 0 || index >= len(e.segments) {
		return fieldErr(CodeState, "segment", index, "valid segment index", "segment index is out of range")
	}
	if e.segments[index].State == StateCommitted {
		return fieldErr(CodeState, "state", StateCommitted, "not committed", "cannot inject a fault into a committed segment")
	}
	e.faults[index] = message
	e.addLogLocked("warn", index, "fault injected: "+message)
	return nil
}

func (e *Engine) ClearFault(index int) {
	e.mu.Lock()
	defer e.mu.Unlock()
	delete(e.faults, index)
}

func (e *Engine) UpdateParams(cfg ProcessConfig) (string, error) {
	cfg = cfg.normalized()
	if err := cfg.Validate(); err != nil {
		return "", err
	}
	e.mu.Lock()
	defer e.mu.Unlock()
	if e.closed {
		return "", wbErr(CodeState, "engine is closed")
	}
	// Tile geometry and worker limits are fixed for the lifetime of the
	// engine's segment map. Algorithm and gamut parameters can change while
	// uncommitted segments remain pending.
	cfg.MaxWorkers = e.cfg.MaxWorkers
	cfg.BlockPixels = e.cfg.BlockPixels
	cfg.PixelBufferLimit = e.cfg.PixelBufferLimit
	cfg.MemoryBudgetMB = e.cfg.MemoryBudgetMB
	cfg.TimeBudgetMS = e.cfg.TimeBudgetMS
	e.cfg = cfg
	nextScale := DecideScale(e.scene.Meta, cfg)
	populateCoarseStats(e.scene, &nextScale, cfg)
	nextScale.TileWidth = e.scale.TileWidth
	nextScale.TileHeight = e.scale.TileHeight
	nextScale.BlockPixels = e.scale.BlockPixels
	nextScale.PixelCount = e.scale.PixelCount
	e.scale = nextScale
	e.paramSerial++
	e.paramVersion = fmt.Sprintf("p%d", e.paramSerial)
	e.addLogLocked("info", -1, "parameters updated to "+e.paramVersion+"; committed segments remain frozen")
	e.cond.Broadcast()
	return e.paramVersion, nil
}

func (e *Engine) Result() (*RenderResult, error) {
	e.mu.Lock()
	defer e.mu.Unlock()
	committed := e.countCommittedLocked()
	if committed == 0 {
		return nil, wbErr(CodeState, "no committed segments are available")
	}
	width, height := e.scene.Meta.Width, e.scene.Meta.Height
	result := &RenderResult{
		SceneID:         e.scene.ID,
		Width:           width,
		Height:          height,
		Path:            "segmented-" + e.scale.SelectedPath,
		Scale:           e.scale,
		FusedRadiance:   make([]float64, width*height),
		LinearRGB:       make([]float64, width*height*3),
		DisplayRGB:      make([]uint8, width*height*3),
		GamutMask:       make([]uint8, width*height),
		GamutBoundaryXY: gamutBoundary(e.scene.Meta.TargetGamut),
	}
	var weightedMean float64
	var committedPixels int
	for _, seg := range e.segments {
		if seg.State != StateCommitted || seg.Snapshot == nil {
			continue
		}
		snap := seg.Snapshot
		for ly := 0; ly < snap.Height; ly++ {
			src := ly * snap.Width
			dst := (snap.Y+ly)*width + snap.X
			copy(result.FusedRadiance[dst:dst+snap.Width], snap.Fused[src:src+snap.Width])
			copy(result.LinearRGB[dst*3:(dst+snap.Width)*3], snap.Chroma[src*3:(src+snap.Width)*3])
			copy(result.DisplayRGB[dst*3:(dst+snap.Width)*3], snap.Display[src*3:(src+snap.Width)*3])
			copy(result.GamutMask[dst:dst+snap.Width], snap.GamutMask[src:src+snap.Width])
		}
		result.PeakRadiance = math.Max(result.PeakRadiance, snap.Peak)
		result.OutOfGamut += snap.OutOfGamut
		weightedMean += snap.Mean * float64(snap.Width*snap.Height)
		committedPixels += snap.Width * snap.Height
		if len(result.OutOfGamutSample) < 512 {
			remaining := 512 - len(result.OutOfGamutSample)
			if len(snap.OutOfGamutSample) > remaining {
				result.OutOfGamutSample = append(result.OutOfGamutSample, snap.OutOfGamutSample[:remaining]...)
			} else {
				result.OutOfGamutSample = append(result.OutOfGamutSample, snap.OutOfGamutSample...)
			}
		}
	}
	if committedPixels > 0 {
		result.MeanRadiance = weightedMean / float64(committedPixels)
	}
	result.OutOfGamutHash = hashGamutMask(result.GamutMask, width)
	result.ElapsedMS = float64(time.Since(e.startedAt).Milliseconds())
	return result, nil
}

func (e *Engine) countCommittedLocked() int {
	count := 0
	for _, seg := range e.segments {
		if seg.State == StateCommitted {
			count++
		}
	}
	return count
}

func (e *Engine) Status() EngineStatus {
	e.mu.Lock()
	defer e.mu.Unlock()
	status := EngineStatus{
		SceneID:        e.scene.ID,
		Running:        e.running,
		Paused:         e.paused,
		Closed:         e.closed,
		Progress:       e.progressLocked(),
		ParamVersion:   e.paramVersion,
		Config:         e.cfg,
		Scale:          e.scale,
		ActiveWorkers:  e.active,
		MaxWorkers:     cap(e.slots),
		BufferCapacity: cap(e.buffers),
		BuffersInUse:   len(e.buffers),
		Checkpoint:     e.checkpoint,
		Logs:           append([]LogEntry(nil), e.logs...),
		ContentVersion: fmt.Sprintf("content-%d", e.contentSerial),
	}
	status.ResultReady = status.Progress.Committed == status.Progress.Total && status.Progress.Total > 0
	for _, seg := range e.segments {
		copySeg := *seg
		copySeg.Snapshot = nil
		if copySeg.Error == nil && seg.State == StateFailed {
			copySeg.Error = wbErr(CodeInternal, "segment failed")
		}
		status.Segments = append(status.Segments, copySeg)
		if status.FirstFailureText == "" && seg.State == StateFailed && seg.Error != nil {
			status.FirstFailureText = seg.Error.Error()
		}
		if seg.State == StateCommitted && seg.Snapshot != nil {
			status.PeakRadiance = math.Max(status.PeakRadiance, seg.Snapshot.Peak)
			status.OutOfGamut += seg.Snapshot.OutOfGamut
		}
	}
	return status
}

func (e *Engine) Wait(ctx context.Context) error {
	ticker := time.NewTicker(10 * time.Millisecond)
	defer ticker.Stop()
	for {
		e.mu.Lock()
		done := !e.running && e.active == 0
		closed := e.closed
		e.mu.Unlock()
		if done || closed {
			return nil
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-ticker.C:
		}
	}
}

func (e *Engine) Run(ctx context.Context) error {
	if err := e.Start(); err != nil {
		return err
	}
	return e.Wait(ctx)
}

func (e *Engine) Close() {
	e.mu.Lock()
	if e.closed {
		e.mu.Unlock()
		return
	}
	e.closed = true
	e.paused = false
	e.cond.Broadcast()
	e.mu.Unlock()
	for {
		e.mu.Lock()
		active := e.active
		e.mu.Unlock()
		if active == 0 {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
}

func (e *Engine) Scene() *Scene {
	return e.scene
}

func (e *Engine) CheckpointReport() CheckpointReport {
	e.mu.Lock()
	defer e.mu.Unlock()
	return e.checkpoint
}

func shortHash(hash string) string {
	if len(hash) > 12 {
		return hash[:12]
	}
	return hash
}
