package engine

import (
	"context"
	"encoding/json"
	"runtime/debug"
	"sync"
	"sync/atomic"
	"time"

	"ms-workbench/internal/apperr"
	"ms-workbench/internal/pipeline"
	"ms-workbench/internal/scene"
	"ms-workbench/internal/store"
)

type Engine struct {
	runMu sync.Mutex
	mu    sync.Mutex

	scene    *scene.Scene
	cp       *store.Checkpoint
	cfg      Config
	segments []*Segment
	running  bool
	paused   atomic.Bool

	snapshotReset        string
	loadedFromCheckpoint bool
	lastInvalidation     *InvalidationReport
	pixelSem             chan struct{}
}

func New(s *scene.Scene, cp *store.Checkpoint, cfg Config) (*Engine, error) {
	if s == nil {
		return nil, apperr.New(apperr.EPARAM, "scene is nil")
	}
	if cp == nil {
		return nil, apperr.New(apperr.EPARAM, "checkpoint is nil")
	}
	if cfg == (Config{}) {
		cfg = DefaultConfig()
	}
	if err := cfg.Validate(); err != nil {
		return nil, apperr.New(apperr.EPARAM, "invalid engine config: %v", err)
	}
	if err := s.Validate(); err != nil {
		return nil, apperr.Schema(s.SourceVersion, "$", "%v", err)
	}
	e := &Engine{
		scene:    s.Clone(),
		cp:       cp,
		cfg:      cfg,
		pixelSem: make(chan struct{}, cfg.MaxWorkers*cfg.MaxPixelsPerWorker),
	}
	e.buildSegmentsLocked()
	key := e.keyLocked()
	records, info, err := cp.Load(key)
	if err != nil {
		e.snapshotReset = err.Error()
		e.resetSegmentsLocked()
		return e, nil
	}
	if len(records) > 0 {
		if err := e.restoreRecordsLocked(records); err != nil {
			_ = cp.ResetAll()
			e.snapshotReset = err.Error()
			e.resetSegmentsLocked()
			return e, nil
		}
		e.loadedFromCheckpoint = true
	}
	_ = info
	return e, nil
}

func (e *Engine) Scene() *scene.Scene {
	e.mu.Lock()
	defer e.mu.Unlock()
	return e.scene.Clone()
}
func (e *Engine) Config() Config { return e.cfg }
func (e *Engine) LoadedFromCheckpoint() bool {
	e.mu.Lock()
	defer e.mu.Unlock()
	return e.loadedFromCheckpoint
}
func (e *Engine) SnapshotResetReason() string {
	e.mu.Lock()
	defer e.mu.Unlock()
	return e.snapshotReset
}

func (e *Engine) Pause()  { e.paused.Store(true) }
func (e *Engine) Resume() { e.paused.Store(false) }

func (e *Engine) buildSegmentsLocked() {
	e.segments = nil
	cols := (e.scene.Width + e.cfg.ChunkWidth - 1) / e.cfg.ChunkWidth
	rows := (e.scene.Height + e.cfg.ChunkHeight - 1) / e.cfg.ChunkHeight
	for cy := 0; cy < rows; cy++ {
		for cx := 0; cx < cols; cx++ {
			x := cx * e.cfg.ChunkWidth
			y := cy * e.cfg.ChunkHeight
			w := minInt(e.cfg.ChunkWidth, e.scene.Width-x)
			h := minInt(e.cfg.ChunkHeight, e.scene.Height-y)
			e.segments = append(e.segments, &Segment{
				ID:          len(e.segments),
				Rect:        scene.Rect{X: x, Y: y, W: w, H: h},
				State:       Pending,
				FusionDirty: true,
				ChromaDirty: true,
			})
		}
	}
}

func (e *Engine) resetSegmentsLocked() {
	for _, seg := range e.segments {
		seg.State = Pending
		seg.Result = nil
		seg.Ready = false
		seg.FusionDirty = true
		seg.ChromaDirty = true
		seg.Reasons = nil
		seg.LastError = ""
		seg.Reused = false
	}
	e.lastInvalidation = nil
}

func (e *Engine) restoreRecordsLocked(records []store.SegmentRecord) error {
	bandCount := len(e.scene.Bands)
	for _, rec := range records {
		if rec.SegmentID < 0 || rec.SegmentID >= len(e.segments) {
			return apperr.New(apperr.EHASH, "checkpoint segment %d is outside the scene", rec.SegmentID)
		}
		seg := e.segments[rec.SegmentID]
		wantFused := seg.Rect.W * seg.Rect.H * bandCount
		wantRGB := seg.Rect.W * seg.Rect.H * 3
		if len(rec.Fused) != wantFused || len(rec.LinearRGB) != wantRGB {
			return apperr.New(apperr.EHASH, "checkpoint segment %d buffer geometry mismatch", rec.SegmentID)
		}
		chroma := &pipeline.ChromaOutput{
			LinearRGB:    append([]float64(nil), rec.LinearRGB...),
			Boundary:     append([]pipeline.BoundaryPoint(nil), rec.Boundary...),
			PeakRadiance: rec.PeakRadiance,
		}
		for _, g := range rec.OutOfGamut {
			x := g % e.scene.Width
			y := g / e.scene.Width
			if !seg.Rect.Contains(x, y) {
				return apperr.New(apperr.EHASH, "checkpoint segment %d contains an out-of-range gamut pixel", rec.SegmentID)
			}
			chroma.OutOfGamut = append(chroma.OutOfGamut, (y-seg.Rect.Y)*seg.Rect.W+(x-seg.Rect.X))
		}
		seg.Result = &pipeline.ChunkResult{
			Fused: &pipeline.FusedChunk{
				Data:         append([]float64(nil), rec.Fused...),
				PeakRadiance: rec.PeakRadiance,
			},
			Chroma: chroma,
		}
		seg.State = Committed
		seg.Ready = false
		seg.FusionDirty = false
		seg.ChromaDirty = false
		seg.Reasons = nil
		seg.LastError = ""
	}
	return nil
}

func (e *Engine) keyLocked() store.Key {
	return store.Key{
		SceneID:          e.scene.SceneID,
		SceneFingerprint: e.scene.Fingerprint(),
		ParamVersion:     e.scene.Settings.Version(),
		BandVersion:      e.scene.BandMetadataVersion(),
		AlgorithmVersion: e.cfg.AlgorithmVersion,
		Width:            e.scene.Width,
		Height:           e.scene.Height,
		BandCount:        len(e.scene.Bands),
		ChunkW:           e.cfg.ChunkWidth,
		ChunkH:           e.cfg.ChunkHeight,
	}
}

func (e *Engine) Run(ctx context.Context, opts RunOptions) (RunReport, error) {
	if opts.Mode == "" {
		opts.Mode = ModeLive
	}
	if opts.MaxNewCommits < 0 {
		return RunReport{}, apperr.New(apperr.EPARAM, "max new commits cannot be negative")
	}
	e.runMu.Lock()
	defer e.runMu.Unlock()
	e.mu.Lock()
	if e.running {
		e.mu.Unlock()
		return RunReport{}, apperr.New(apperr.ESTATE, "engine is already running")
	}
	e.running = true
	e.paused.Store(false)
	e.mu.Unlock()
	defer func() {
		e.mu.Lock()
		for _, seg := range e.segments {
			if seg.State == Processing {
				_ = transitionLocked(&seg.State, Pending, seg.ID)
				seg.Ready = false
				seg.Result = nil
				seg.FusionDirty = true
				seg.ChromaDirty = true
			}
		}
		e.running = false
		e.mu.Unlock()
	}()

	if opts.Mode == ModeFull {
		if err := e.cp.ResetAll(); err != nil {
			return RunReport{}, err
		}
		e.mu.Lock()
		e.resetSegmentsLocked()
		e.snapshotReset = ""
		e.loadedFromCheckpoint = false
		e.mu.Unlock()
	}

	newCommits := 0
	for {
		if e.paused.Load() || ctx.Err() != nil {
			break
		}
		e.mu.Lock()
		if e.hasFailedLocked() {
			e.mu.Unlock()
			break
		}
		limit := e.cfg.MaxWorkers
		if opts.MaxNewCommits > 0 {
			remaining := opts.MaxNewCommits - newCommits
			if remaining <= 0 {
				e.mu.Unlock()
				break
			}
			if remaining < limit {
				limit = remaining
			}
		}
		ids := e.pendingIDsLocked(limit)
		if len(ids) == 0 {
			e.mu.Unlock()
			break
		}
		for _, id := range ids {
			seg := e.segments[id]
			seg.LastError = ""
			if err := transitionLocked(&seg.State, Processing, id); err != nil {
				e.mu.Unlock()
				return RunReport{}, err
			}
		}
		key := e.keyLocked()
		e.mu.Unlock()

		results := e.processBatch(ctx, ids)
		committedThisBatch := 0
		for _, wr := range results {
			e.mu.Lock()
			seg := e.segments[wr.id]
			if wr.err != nil {
				seg.LastError = wr.err.Error()
				_ = transitionLocked(&seg.State, Failed, seg.ID)
				seg.Ready = false
			} else {
				seg.Result = wr.result
				seg.Ready = true
				seg.FusionDirty = false
				seg.ChromaDirty = false
			}
			e.mu.Unlock()
		}

		// Commits are serialized by the SQLite layer and always advance the
		// committed prefix in ascending segment order.
		for {
			e.mu.Lock()
			seg := e.firstUncommittedLocked()
			if seg == nil || seg.State == Failed || !seg.Ready || seg.State != Processing {
				e.mu.Unlock()
				break
			}
			rec, err := e.recordForSegmentLocked(seg)
			e.mu.Unlock()
			if err != nil {
				return e.finishReport(opts.Mode, newCommits, err), err
			}
			_, err = e.cp.CommitSegment(key, rec)
			e.mu.Lock()
			if err != nil {
				seg.LastError = err.Error()
				_ = transitionLocked(&seg.State, Failed, seg.ID)
				seg.Ready = false
				e.mu.Unlock()
				return e.finishReport(opts.Mode, newCommits, err), err
			}
			if err := transitionLocked(&seg.State, Committed, seg.ID); err != nil {
				e.mu.Unlock()
				return e.finishReport(opts.Mode, newCommits, err), err
			}
			seg.Ready = false
			seg.FusionDirty = false
			seg.ChromaDirty = false
			newCommits++
			committedThisBatch++
			e.mu.Unlock()
			if opts.MaxNewCommits > 0 && newCommits >= opts.MaxNewCommits {
				break
			}
		}

		e.mu.Lock()
		for _, id := range ids {
			seg := e.segments[id]
			if seg.State == Processing {
				_ = transitionLocked(&seg.State, Pending, id)
				seg.Ready = false
				seg.Result = nil
				seg.FusionDirty = true
				seg.ChromaDirty = true
			}
		}
		e.mu.Unlock()
		_ = committedThisBatch
	}
	report := e.finishReport(opts.Mode, newCommits, nil)
	if err := ctx.Err(); err != nil {
		return report, err
	}
	return report, nil
}

type workResult struct {
	id     int
	result *pipeline.ChunkResult
	err    error
}

func (e *Engine) processBatch(ctx context.Context, ids []int) []workResult {
	out := make([]workResult, len(ids))
	var wg sync.WaitGroup
	for i, id := range ids {
		wg.Add(1)
		go func(slot, segmentID int) {
			defer wg.Done()
			defer func() {
				if r := recover(); r != nil {
					out[slot] = workResult{id: segmentID, err: apperr.New(apperr.EPARAM, "segment worker panic: %v\n%s", r, debug.Stack())}
				}
			}()
			result, err := e.processSegment(ctx, segmentID)
			out[slot] = workResult{id: segmentID, result: result, err: err}
		}(i, id)
	}
	wg.Wait()
	return out
}

func (e *Engine) processSegment(ctx context.Context, id int) (*pipeline.ChunkResult, error) {
	e.mu.Lock()
	seg := e.segments[id]
	rect := seg.Rect
	reuseFused := !seg.FusionDirty && seg.Result != nil && seg.Result.Fused != nil
	var fused *pipeline.FusedChunk
	if reuseFused {
		fused = seg.Result.Fused
	}
	settings := e.scene.Settings
	e.mu.Unlock()

	pixels := rect.W * rect.H
	if pixels > e.cfg.MaxPixelsPerWorker {
		return nil, apperr.New(apperr.EPARAM, "segment %d requires %d pixels, per-worker buffer is %d", id, pixels, e.cfg.MaxPixelsPerWorker)
	}
	if pixels > cap(e.pixelSem) {
		return nil, apperr.New(apperr.EPARAM, "segment %d buffer request cannot ever be satisfied", id)
	}
	if e.cfg.ChunkDelayMS > 0 {
		select {
		case <-ctx.Done():
		case <-time.After(time.Duration(e.cfg.ChunkDelayMS) * time.Millisecond):
		}
	}
	acquired := 0
	for n := 0; n < pixels; n++ {
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case e.pixelSem <- struct{}{}:
			acquired++
		}
	}
	defer func() {
		for n := 0; n < acquired; n++ {
			<-e.pixelSem
		}
	}()

	if reuseFused {
		chroma := pipeline.ChromaChunk(e.scene, rect, fused, settings)
		return &pipeline.ChunkResult{Fused: fused, Chroma: chroma}, nil
	}
	return pipeline.ProcessChunk(e.scene, rect, settings)
}

func (e *Engine) pendingIDsLocked(limit int) []int {
	ids := make([]int, 0, limit)
	for _, seg := range e.segments {
		if seg.State == Pending && !seg.Ready {
			ids = append(ids, seg.ID)
			if len(ids) == limit {
				break
			}
		}
	}
	return ids
}

func (e *Engine) hasFailedLocked() bool {
	for _, seg := range e.segments {
		if seg.State == Failed {
			return true
		}
	}
	return false
}

func (e *Engine) firstUncommittedLocked() *Segment {
	for _, seg := range e.segments {
		if seg.State != Committed {
			return seg
		}
	}
	return nil
}

func (e *Engine) recordForSegmentLocked(seg *Segment) (store.SegmentRecord, error) {
	if seg.Result == nil || seg.Result.Fused == nil || seg.Result.Chroma == nil {
		return store.SegmentRecord{}, apperr.New(apperr.EPARAM, "segment %d has no complete result", seg.ID)
	}
	oog := make([]int, len(seg.Result.Chroma.OutOfGamut))
	for i, local := range seg.Result.Chroma.OutOfGamut {
		lx := local % seg.Rect.W
		ly := local / seg.Rect.W
		oog[i] = (seg.Rect.Y+ly)*e.scene.Width + seg.Rect.X + lx
	}
	boundary, err := json.Marshal(seg.Result.Chroma.Boundary)
	if err != nil {
		return store.SegmentRecord{}, apperr.New(apperr.EIO, "encode gamut boundary: %v", err)
	}
	return store.SegmentRecord{
		SegmentID:    seg.ID,
		Fused:        append([]float64(nil), seg.Result.Fused.Data...),
		LinearRGB:    append([]float64(nil), seg.Result.Chroma.LinearRGB...),
		OutOfGamut:   oog,
		Boundary:     append([]pipeline.BoundaryPoint(nil), seg.Result.Chroma.Boundary...),
		BoundaryJSON: boundary,
		PeakRadiance: seg.Result.Fused.PeakRadiance,
	}, nil
}

func (e *Engine) finishReport(mode RunMode, newCommits int, _ error) RunReport {
	view := e.View()
	return RunReport{
		Mode:           mode,
		Complete:       view.Committed == view.Total,
		NewCommits:     newCommits,
		Committed:      view.Committed,
		Total:          view.Total,
		CommitSequence: view.CommitSequence,
		SnapshotReset:  view.SnapshotReset,
		Paused:         view.Paused,
	}
}

func (e *Engine) RetrySegment(ctx context.Context, id int) (RunReport, error) {
	e.runMu.Lock()
	defer e.runMu.Unlock()
	e.mu.Lock()
	if e.running {
		e.mu.Unlock()
		return RunReport{}, apperr.New(apperr.ESTATE, "engine is running")
	}
	if id < 0 || id >= len(e.segments) {
		e.mu.Unlock()
		return RunReport{}, apperr.New(apperr.EPARAM, "segment %d does not exist", id)
	}
	seg := e.segments[id]
	if seg.State != Failed {
		err := &apperr.Error{Code: apperr.ESTATE, Message: "only failed segments can be retried", Segment: id, FromState: string(seg.State), ToState: string(Processing)}
		e.mu.Unlock()
		return RunReport{}, err
	}
	if err := transitionLocked(&seg.State, Processing, id); err != nil {
		e.mu.Unlock()
		return RunReport{}, err
	}
	seg.LastError = ""
	e.running = true
	key := e.keyLocked()
	e.mu.Unlock()
	defer func() {
		e.mu.Lock()
		e.running = false
		e.mu.Unlock()
	}()

	wr := e.processBatch(ctx, []int{id})
	e.mu.Lock()
	result := wr[0]
	seg = e.segments[id]
	if result.err != nil {
		seg.LastError = result.err.Error()
		_ = transitionLocked(&seg.State, Failed, id)
		e.mu.Unlock()
		return e.finishReport(ModeResume, 0, result.err), result.err
	}
	seg.Result = result.result
	seg.Ready = true
	seg.FusionDirty = false
	seg.ChromaDirty = false
	rec, err := e.recordForSegmentLocked(seg)
	e.mu.Unlock()
	if err != nil {
		return e.finishReport(ModeResume, 0, err), err
	}
	if _, err := e.cp.CommitSegment(key, rec); err != nil {
		e.mu.Lock()
		seg.LastError = err.Error()
		_ = transitionLocked(&seg.State, Failed, id)
		e.mu.Unlock()
		return e.finishReport(ModeResume, 0, err), err
	}
	e.mu.Lock()
	_ = transitionLocked(&seg.State, Committed, id)
	seg.Ready = false
	e.mu.Unlock()
	return e.finishReport(ModeResume, 1, nil), nil
}

func (e *Engine) Composite() *Composite {
	e.mu.Lock()
	defer e.mu.Unlock()
	bandCount := len(e.scene.Bands)
	out := &Composite{
		Width:        e.scene.Width,
		Height:       e.scene.Height,
		BandIDs:      scene.SortedBandIDs(e.scene.Bands),
		Fused:        make([]float64, e.scene.Width*e.scene.Height*bandCount),
		LinearRGB:    make([]float64, e.scene.Width*e.scene.Height*3),
		SegmentPeaks: make([]float64, len(e.segments)),
		Total:        len(e.segments),
		Complete:     true,
	}
	e.orderedBandIDsLocked() // keep band order explicit and stable in Scene, not sorted IDs
	out.BandIDs = make([]string, bandCount)
	for i, b := range e.scene.Bands {
		out.BandIDs[i] = b.ID
	}
	for _, seg := range e.segments {
		if seg.State != Committed || seg.Result == nil || seg.Result.Fused == nil || seg.Result.Chroma == nil {
			out.Complete = false
			continue
		}
		out.Committed++
		out.CommitSequence = append(out.CommitSequence, seg.ID)
		fused := seg.Result.Fused.Data
		rgb := seg.Result.Chroma.LinearRGB
		for ly := 0; ly < seg.Rect.H; ly++ {
			for lx := 0; lx < seg.Rect.W; lx++ {
				src := (ly*seg.Rect.W + lx) * bandCount
				dst := ((seg.Rect.Y+ly)*e.scene.Width + seg.Rect.X + lx) * bandCount
				copy(out.Fused[dst:dst+bandCount], fused[src:src+bandCount])
				src3 := (ly*seg.Rect.W + lx) * 3
				dst3 := ((seg.Rect.Y+ly)*e.scene.Width + seg.Rect.X + lx) * 3
				copy(out.LinearRGB[dst3:dst3+3], rgb[src3:src3+3])
			}
		}
		for _, local := range seg.Result.Chroma.OutOfGamut {
			out.OutOfGamut = append(out.OutOfGamut, Point{
				X: seg.Rect.X + local%seg.Rect.W,
				Y: seg.Rect.Y + local/seg.Rect.W,
			})
		}
		out.GamutBoundary = append(out.GamutBoundary, seg.Result.Chroma.Boundary...)
		peak := seg.Result.Fused.PeakRadiance
		out.SegmentPeaks[seg.ID] = peak
		if peak > out.PeakRadiance {
			out.PeakRadiance = peak
		}
	}
	sortInts(out.CommitSequence)
	out.Complete = out.Committed == out.Total
	return out
}

func (e *Engine) orderedBandIDsLocked() {}

func (e *Engine) PatchMeasurements() ([]pipeline.PatchMeasurement, error) {
	comp := e.Composite()
	if !comp.Complete {
		return nil, apperr.New(apperr.ESTATE, "composite is incomplete")
	}
	return pipeline.MeasurePatches(e.scene, comp.LinearRGB), nil
}

func (e *Engine) View() View {
	e.mu.Lock()
	defer e.mu.Unlock()
	v := View{
		SceneID:          e.scene.SceneID,
		SceneName:        e.scene.Name,
		SourceVersion:    e.scene.SourceVersion,
		Width:            e.scene.Width,
		Height:           e.scene.Height,
		BandIDs:          make([]string, len(e.scene.Bands)),
		Settings:         e.scene.Settings,
		ParamVersion:     e.scene.Settings.Version(),
		BandVersion:      e.scene.BandMetadataVersion(),
		AlgorithmVersion: e.cfg.AlgorithmVersion,
		Total:            len(e.segments),
		Running:          e.running,
		Paused:           e.paused.Load(),
		SnapshotReset:    e.snapshotReset,
		Fingerprint:      e.scene.Fingerprint(),
	}
	for i, b := range e.scene.Bands {
		v.BandIDs[i] = b.ID
	}
	for _, seg := range e.segments {
		sv := SegmentView{
			ID:      seg.ID,
			State:   seg.State,
			X:       seg.Rect.X,
			Y:       seg.Rect.Y,
			W:       seg.Rect.W,
			H:       seg.Rect.H,
			Error:   seg.LastError,
			Reasons: append([]string(nil), seg.Reasons...),
			Reused:  seg.Reused,
		}
		if seg.Result != nil && seg.Result.Chroma != nil {
			sv.OutOfGamut = len(seg.Result.Chroma.OutOfGamut)
		}
		if seg.Result != nil && seg.Result.Fused != nil {
			sv.PeakRadiance = seg.Result.Fused.PeakRadiance
		}
		if seg.State == Committed {
			v.Committed++
			v.CommitSequence = append(v.CommitSequence, seg.ID)
		}
		v.Segments = append(v.Segments, sv)
	}
	sortInts(v.CommitSequence)
	if e.lastInvalidation != nil {
		cp := *e.lastInvalidation
		cp.ChangedFields = append([]string(nil), e.lastInvalidation.ChangedFields...)
		cp.Segments = append([]SegmentInvalidation(nil), e.lastInvalidation.Segments...)
		v.LastInvalidation = &cp
	}
	return v
}

func minInt(a, b int) int {
	if a < b {
		return a
	}
	return b
}

func maxInt(a, b int) int {
	if a > b {
		return a
	}
	return b
}
