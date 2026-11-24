package workbench

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"image/color"
	"image/png"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"edgeworkbench/internal/core"
)

type SampleSummary struct {
	Name           string  `json:"name"`
	Width          int     `json:"width"`
	Height         int     `json:"height"`
	Channels       int     `json:"channels"`
	Noisy          bool    `json:"noisy"`
	LowContrast    bool    `json:"lowContrast"`
	Fingerprint    string  `json:"fingerprint"`
	EdgeCoverage   float64 `json:"edgeCoverage"`
	LineCount      int     `json:"lineCount"`
	CircleCount    int     `json:"circleCount"`
	ReferenceCount int     `json:"referenceCount"`
}

type AppState struct {
	AlgorithmVersion string          `json:"algorithmVersion"`
	CheckpointPath   string          `json:"checkpointPath"`
	DataDirectory    string          `json:"dataDirectory"`
	Samples          []SampleSummary `json:"samples"`
	Session          *SessionView    `json:"session,omitempty"`
	Defaults         core.Params     `json:"defaults"`
	Tolerance        core.Tolerance  `json:"tolerance"`
	State            string          `json:"state"`
}

type SessionView struct {
	Name         string              `json:"name"`
	Manifest     core.SampleManifest `json:"manifest"`
	ImageURL     string              `json:"imageUrl"`
	ParamVersion string              `json:"paramVersion"`
	Params       core.Params         `json:"params"`
}

type StageView struct {
	Stage       string  `json:"stage"`
	Coverage    float64 `json:"coverage"`
	Fingerprint string  `json:"fingerprint"`
	DurationMS  float64 `json:"durationMs"`
	MaskURL     string  `json:"maskUrl"`
}

type RunView struct {
	Mode                  string                  `json:"mode"`
	Status                string                  `json:"status"`
	ImageFingerprint      string                  `json:"imageFingerprint"`
	ParamVersion          string                  `json:"paramVersion"`
	AlgorithmVersion      string                  `json:"algorithmVersion"`
	Params                core.Params             `json:"params"`
	Stages                []StageView             `json:"stages"`
	Stats                 core.Geometry           `json:"stats"`
	TruthDeviation        float64                 `json:"truthDeviation"`
	CommitSequence        []core.CommitKey        `json:"commitSequence"`
	BlockStates           []core.StageBlockMatrix `json:"blockStates"`
	FailedBlocks          []core.CommitKey        `json:"failedBlocks"`
	CheckpointInvalidated bool                    `json:"checkpointInvalidated"`
	CheckpointReason      string                  `json:"checkpointReason,omitempty"`
	RestoredCommits       int                     `json:"restoredCommits"`
	PoolStats             core.PoolStats          `json:"poolStats"`
	ElapsedMS             float64                 `json:"elapsedMs"`
	FrameState            string                  `json:"frameState"`
	HeatmapURL            string                  `json:"heatmapUrl,omitempty"`
}

type PreviewView struct {
	State          string             `json:"state"`
	ParamVersion   string             `json:"paramVersion"`
	Params         core.Params        `json:"params"`
	Policy         core.PreviewPolicy `json:"policy"`
	ElapsedMS      float64            `json:"elapsedMs"`
	MaskWidth      int                `json:"maskWidth"`
	MaskHeight     int                `json:"maskHeight"`
	MaskURL        string             `json:"maskUrl"`
	Stats          core.Geometry      `json:"stats"`
	TruthDeviation float64            `json:"truthDeviation"`
	Tolerance      core.Tolerance     `json:"tolerance"`
	Placeholder    bool               `json:"placeholder"`
}

type PreviewResponse struct {
	View  *PreviewView   `json:"view,omitempty"`
	Error *core.AppError `json:"error,omitempty"`
}

type ExactRequest struct {
	Params          core.Params `json:"params"`
	ExpectedVersion string      `json:"expectedVersion"`
}

type ExactResponse struct {
	View       *RunView         `json:"view,omitempty"`
	Divergence *core.Divergence `json:"divergence,omitempty"`
	Error      *core.AppError   `json:"error,omitempty"`
}

type RunRequest struct {
	Mode   string      `json:"mode"`
	Params core.Params `json:"params"`
}

type RunResponse struct {
	View  *RunView       `json:"view,omitempty"`
	Error *core.AppError `json:"error,omitempty"`
}

type RetryRequest struct {
	Stage string `json:"stage"`
	Block int    `json:"block"`
}

type ExportResult struct {
	Path      string `json:"path"`
	FileCount int    `json:"fileCount"`
}

type previewRecord struct {
	fullMask  core.Image
	stats     core.Geometry
	policy    core.PreviewPolicy
	params    core.Params
	version   string
	elapsedMS float64
	display   core.Image
	createdAt time.Time
}

type Workbench struct {
	mu           sync.Mutex
	opMu         sync.Mutex
	baseCtx      context.Context
	dataDir      string
	sampleDir    string
	store        *core.CheckpointStore
	samples      []core.LoadedSample
	current      *core.LoadedSample
	params       core.Params
	preview      *previewRecord
	lastRun      *core.RunResult
	lastRunView  *RunView
	state        string
	exactRunning bool
	control      *core.RunControl
	cancel       context.CancelFunc
	tolerance    core.Tolerance
}

func New(dataDir string) *Workbench {
	ctx, cancel := context.WithCancel(context.Background())
	_ = cancel
	return &Workbench{
		baseCtx:   ctx,
		dataDir:   dataDir,
		sampleDir: filepath.Join(dataDir, "samples"),
		params:    core.DefaultParams(),
		state:     "idle",
		tolerance: core.DefaultTolerance(),
	}
}

func (w *Workbench) Bootstrap() (AppState, error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	if err := os.MkdirAll(w.sampleDir, 0o755); err != nil {
		return AppState{}, core.WrapError(core.CodeCheckpoint, "dataDirectory", "cannot create data directory", err)
	}
	store, err := core.OpenCheckpointStore(filepath.Join(w.dataDir, "checkpoints", "workbench.db"))
	if err != nil {
		return AppState{}, err
	}
	if w.store != nil {
		_ = w.store.Close()
	}
	w.store = store
	w.samples = nil
	for _, spec := range core.DefaultSampleSpecs() {
		dir := filepath.Join(w.sampleDir, spec.Name)
		sample, err := core.LoadSample(dir)
		if err != nil {
			sample, err = core.GenerateSample(w.sampleDir, spec)
			if err != nil {
				return AppState{}, err
			}
		}
		w.samples = append(w.samples, sample)
	}
	if w.current == nil && len(w.samples) > 0 {
		w.current = &w.samples[0]
	}
	state := w.stateLocked()
	return state, nil
}

func (w *Workbench) stateLocked() AppState {
	state := AppState{
		AlgorithmVersion: core.AlgorithmVersion,
		DataDirectory:    w.dataDir,
		Defaults:         core.DefaultParams(),
		Tolerance:        w.tolerance,
		State:            w.state,
	}
	if w.store != nil {
		state.CheckpointPath = w.store.Path()
	}
	for _, sample := range w.samples {
		state.Samples = append(state.Samples, summarize(sample))
	}
	if w.current != nil {
		session := w.sessionLocked()
		state.Session = &session
	}
	return state
}

func summarize(sample core.LoadedSample) SampleSummary {
	m := sample.Manifest
	return SampleSummary{
		Name:           m.Name,
		Width:          m.Width,
		Height:         m.Height,
		Channels:       m.Channels,
		Noisy:          m.Noisy,
		LowContrast:    m.LowContrast,
		Fingerprint:    m.Fingerprint,
		EdgeCoverage:   m.Truth.EdgeCoverage,
		LineCount:      len(m.Truth.Lines),
		CircleCount:    len(m.Truth.Circles),
		ReferenceCount: len(m.References),
	}
}

func (w *Workbench) sessionLocked() SessionView {
	m := w.current.Manifest
	return SessionView{
		Name:         m.Name,
		Manifest:     m,
		ImageURL:     dataURL(w.current.Image, 900, false),
		ParamVersion: w.params.Version(),
		Params:       w.params,
	}
}

func (w *Workbench) LoadSample(name string) (SessionView, error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.exactRunning {
		return SessionView{}, core.NewError(core.CodeState, "frameState", "cannot switch samples while a run is active")
	}
	for i := range w.samples {
		if w.samples[i].Manifest.Name == name {
			w.current = &w.samples[i]
			w.preview = nil
			w.lastRun = nil
			w.lastRunView = nil
			w.state = "idle"
			return w.sessionLocked(), nil
		}
	}
	return SessionView{}, core.NewError(core.CodeMeta, "sample", "unknown sample "+name)
}

func (w *Workbench) Preview(params core.Params) PreviewResponse {
	w.mu.Lock()
	defer w.mu.Unlock()
	started := time.Now()
	if w.current == nil {
		return PreviewResponse{Error: toAppError(core.NewError(core.CodeState, "session", "no sample loaded"))}
	}
	if w.exactRunning {
		return PreviewResponse{Error: toAppError(core.NewError(core.CodeState, "frameState", "preview rejected while exact recompute is running"))}
	}
	outcome, err := core.ComputePreview(w.current.Image, params, w.current.Manifest.Truth)
	if err != nil {
		return PreviewResponse{Error: toAppError(err)}
	}
	w.preview = &previewRecord{
		fullMask:  outcome.Mask,
		stats:     outcome.Stats,
		policy:    outcome.Policy,
		params:    params,
		version:   outcome.ParamVersion,
		elapsedMS: outcome.ElapsedMS,
		display:   outcome.DisplayMask,
		createdAt: time.Now(),
	}
	w.params = params
	w.state = "preview"
	view := PreviewView{
		State:          "preview",
		ParamVersion:   outcome.ParamVersion,
		Params:         params,
		Policy:         outcome.Policy,
		ElapsedMS:      float64(time.Since(started).Microseconds()) / 1000.0,
		MaskWidth:      outcome.DisplayMask.Width,
		MaskHeight:     outcome.DisplayMask.Height,
		MaskURL:        dataURL(outcome.DisplayMask, 900, true),
		Stats:          outcome.Stats,
		TruthDeviation: core.GeometryTrueDeviation(outcome.Stats),
		Tolerance:      w.tolerance,
		Placeholder:    outcome.Policy.Simplified || outcome.Policy.DownsampleFactor > 1,
	}
	w.preview.elapsedMS = view.ElapsedMS
	return PreviewResponse{View: &view}
}

func (w *Workbench) Exact(req ExactRequest) ExactResponse {
	w.mu.Lock()
	if w.current == nil {
		w.mu.Unlock()
		return ExactResponse{Error: toAppError(core.NewError(core.CodeState, "session", "no sample loaded"))}
	}
	if err := req.Params.Validate(); err != nil {
		w.mu.Unlock()
		return ExactResponse{Error: toAppError(err)}
	}
	actualVersion := req.Params.Version()
	if req.ExpectedVersion != "" && req.ExpectedVersion != actualVersion {
		w.mu.Unlock()
		return ExactResponse{Error: toAppError(&core.AppError{
			Code: core.CodeState, Field: "paramVersion",
			Message: "exact recompute version does not match the locked preview snapshot",
			Details: map[string]any{"expected": req.ExpectedVersion, "actual": actualVersion},
		})}
	}
	if w.preview != nil && w.preview.version != actualVersion {
		w.mu.Unlock()
		return ExactResponse{Error: toAppError(&core.AppError{
			Code: core.CodeState, Field: "paramVersion",
			Message: "last preview belongs to a different parameter version",
			Details: map[string]any{"previewVersion": w.preview.version, "actual": actualVersion},
		})}
	}
	sample := *w.current
	preview := w.preview
	w.exactRunning = true
	w.state = "exact"
	ctx, cancel := context.WithCancel(w.baseCtx)
	w.cancel = cancel
	w.control = core.NewRunControl()
	control := w.control
	w.mu.Unlock()
	defer func() {
		cancel()
		w.mu.Lock()
		w.exactRunning = false
		w.cancel = nil
		w.mu.Unlock()
	}()

	run, err := w.runLocked(ctx, sample, req.Params, core.RunRealtime, control, nil)
	if err != nil {
		return ExactResponse{Error: toAppError(err)}
	}
	var divergence *core.Divergence
	var heatmapURL string
	if preview != nil {
		d := core.ComparePreviewExact(preview.fullMask, run.Final, preview.stats, run.Stats, w.tolerance)
		divergence = &d
		heatmapURL = dataURL(diffHeatmap(preview.fullMask, run.Final), 900, true)
		if !d.Pass {
			view := w.runViewLocked(run, "exact-diverge", heatmapURL, sample)
			w.mu.Lock()
			w.lastRun = &run
			w.lastRunView = &view
			w.params = req.Params
			w.state = "diverge"
			w.mu.Unlock()
			details := map[string]any{
				"coverageDeviation": d.CoverageDeviation,
				"geometryDeviation": d.GeometryDeviation,
				"pixelDiffFraction": d.PixelDiffFraction,
				"tolerance":         d.Tolerance,
			}
			if d.FirstMismatch != nil {
				details["firstMismatch"] = d.FirstMismatch
			}
			return ExactResponse{
				View:       &view,
				Divergence: divergence,
				Error: toAppError(&core.AppError{
					Code: core.CodeDiverge, Field: "previewExact",
					Message: "preview and exact masks exceed declared tolerance",
					Details: details,
				}),
			}
		}
	}
	view := w.runViewLocked(run, "exact", heatmapURL, sample)
	w.mu.Lock()
	w.lastRun = &run
	w.lastRunView = &view
	w.params = req.Params
	w.state = "exact"
	w.mu.Unlock()
	return ExactResponse{View: &view, Divergence: divergence}
}

func (w *Workbench) Run(req RunRequest) RunResponse {
	w.mu.Lock()
	if w.current == nil {
		w.mu.Unlock()
		return RunResponse{Error: toAppError(core.NewError(core.CodeState, "session", "no sample loaded"))}
	}
	if err := req.Params.Validate(); err != nil {
		w.mu.Unlock()
		return RunResponse{Error: toAppError(err)}
	}
	sample := *w.current
	mode, err := parseMode(req.Mode)
	if err != nil {
		w.mu.Unlock()
		return RunResponse{Error: toAppError(err)}
	}
	if w.exactRunning {
		w.mu.Unlock()
		return RunResponse{Error: toAppError(core.NewError(core.CodeState, "frameState", "another exact run is active"))}
	}
	w.exactRunning = true
	w.state = "running"
	ctx, cancel := context.WithCancel(w.baseCtx)
	w.cancel = cancel
	w.control = core.NewRunControl()
	control := w.control
	w.mu.Unlock()
	defer func() {
		cancel()
		w.mu.Lock()
		w.exactRunning = false
		w.cancel = nil
		w.mu.Unlock()
	}()
	run, err := w.runLocked(ctx, sample, req.Params, mode, control, nil)
	if err != nil {
		return RunResponse{Error: toAppError(err)}
	}
	heatmap := ""
	if w.preview != nil && w.preview.version == req.Params.Version() {
		heatmap = dataURL(diffHeatmap(w.preview.fullMask, run.Final), 900, true)
	}
	view := w.runViewLocked(run, string(mode), heatmap, sample)
	w.mu.Lock()
	w.lastRun = &run
	w.lastRunView = &view
	w.params = req.Params
	w.state = string(mode)
	w.mu.Unlock()
	return RunResponse{View: &view}
}

func (w *Workbench) runLocked(ctx context.Context, sample core.LoadedSample, params core.Params, mode core.RunMode, control *core.RunControl, faults map[core.CommitKey]string) (core.RunResult, error) {
	w.mu.Lock()
	store := w.store
	w.mu.Unlock()
	opts := core.RunOptions{
		Mode:            mode,
		Workers:         4,
		PerWorkerPixels: 512 * 1024,
		Checkpoint:      store,
		Faults:          faults,
		Control:         control,
	}
	return core.RunPipeline(ctx, sample.Image, params, sample.Manifest.Truth, opts)
}

func (w *Workbench) runViewLocked(run core.RunResult, frameState, heatmap string, sample core.LoadedSample) RunView {
	view := RunView{
		Mode:                  string(run.Mode),
		Status:                run.Status,
		ImageFingerprint:      run.ImageFingerprint,
		ParamVersion:          run.ParamVersion,
		AlgorithmVersion:      run.AlgorithmVersion,
		Params:                run.Params,
		Stats:                 run.Stats,
		TruthDeviation:        run.TruthDeviation,
		CommitSequence:        run.CommitSequence,
		BlockStates:           run.BlockStates,
		FailedBlocks:          run.FailedBlocks,
		CheckpointInvalidated: run.CheckpointInvalidated,
		CheckpointReason:      run.CheckpointReason,
		RestoredCommits:       run.RestoredCommits,
		PoolStats:             run.PoolStats,
		ElapsedMS:             run.ElapsedMS,
		FrameState:            frameState,
		HeatmapURL:            heatmap,
	}
	for _, stage := range run.Stages {
		view.Stages = append(view.Stages, StageView{
			Stage:       stage.Stage,
			Coverage:    stage.Coverage,
			Fingerprint: stage.Fingerprint,
			DurationMS:  stage.DurationMS,
			MaskURL:     dataURL(stage.Image(), 420, true),
		})
	}
	_ = sample
	return view
}

func (w *Workbench) SetPaused(paused bool) {
	w.mu.Lock()
	control := w.control
	w.mu.Unlock()
	if control != nil {
		control.SetPaused(paused)
	}
}

func (w *Workbench) Cancel() {
	w.mu.Lock()
	cancel := w.cancel
	w.mu.Unlock()
	if cancel != nil {
		cancel()
	}
}

func (w *Workbench) RetryBlock(req RetryRequest) RunResponse {
	w.mu.Lock()
	if w.exactRunning {
		w.mu.Unlock()
		return RunResponse{Error: toAppError(core.NewError(core.CodeState, "frameState", "another run is active"))}
	}
	params := w.params
	sample := w.current
	last := w.lastRun
	w.mu.Unlock()
	if sample == nil || last == nil {
		return RunResponse{Error: toAppError(core.NewError(core.CodeState, "run", "no run available to retry"))}
	}
	valid := false
	for _, matrix := range last.BlockStates {
		if matrix.Stage == req.Stage && req.Block >= 0 && req.Block < len(matrix.States) {
			valid = true
			if matrix.States[req.Block] != core.BlockFailed {
				return RunResponse{Error: toAppError(&core.AppError{
					Code: core.CodeState, Field: "block",
					Message: "only failed blocks can be retried",
					Details: map[string]any{"stage": req.Stage, "block": req.Block, "state": matrix.States[req.Block]},
				})}
			}
		}
	}
	if !valid {
		return RunResponse{Error: toAppError(core.NewError(core.CodeState, "block", "unknown block"))}
	}
	w.mu.Lock()
	if w.exactRunning || w.current == nil || w.current.Manifest.Fingerprint != sample.Manifest.Fingerprint {
		w.mu.Unlock()
		return RunResponse{Error: toAppError(core.NewError(core.CodeState, "frameState", "session changed before retry could start"))}
	}
	w.exactRunning = true
	w.state = "retry"
	ctx, cancel := context.WithCancel(w.baseCtx)
	w.cancel = cancel
	control := core.NewRunControl()
	w.control = control
	w.mu.Unlock()
	defer func() {
		cancel()
		w.mu.Lock()
		w.exactRunning = false
		w.cancel = nil
		w.mu.Unlock()
	}()
	run, err := w.runLocked(ctx, *sample, params, core.RunRealtime, control, nil)
	if err != nil {
		return RunResponse{Error: toAppError(err)}
	}
	view := w.runViewLocked(run, "retry", "", *sample)
	w.mu.Lock()
	w.lastRun = &run
	w.lastRunView = &view
	w.mu.Unlock()
	return RunResponse{View: &view}
}

func (w *Workbench) ExportDiagnostics() (ExportResult, error) {
	w.mu.Lock()
	if w.lastRun == nil || w.current == nil || w.lastRunView == nil {
		w.mu.Unlock()
		return ExportResult{}, core.NewError(core.CodeState, "export", "no completed run to export")
	}
	run := *w.lastRun
	view := *w.lastRunView
	sample := *w.current
	w.mu.Unlock()

	dir := filepath.Join(w.dataDir, "exports", time.Now().Format("20060102-150405.000"))
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return ExportResult{}, core.WrapError(core.CodeCheckpoint, "export", "cannot create export directory", err)
	}
	files := 0
	writePNG := func(name string, im core.Image) error {
		f, err := os.Create(filepath.Join(dir, name))
		if err != nil {
			return err
		}
		if err := core.EncodePNG(f, im); err != nil {
			_ = f.Close()
			return err
		}
		if err := f.Close(); err != nil {
			return err
		}
		files++
		return nil
	}
	if err := writePNG("source.png", sample.Image); err != nil {
		return ExportResult{}, core.WrapError(core.CodeCheckpoint, "export", "cannot export source", err)
	}
	for _, stage := range run.Stages {
		if err := writePNG(stage.Stage+".png", stage.Image()); err != nil {
			return ExportResult{}, core.WrapError(core.CodeCheckpoint, "export", "cannot export "+stage.Stage, err)
		}
	}
	payload := map[string]any{
		"algorithmVersion": core.AlgorithmVersion,
		"sample":           sample.Manifest,
		"run":              view,
		"truthDeviation":   run.TruthDeviation,
		"exportedAt":       time.Now().Format(time.RFC3339Nano),
	}
	raw, err := json.MarshalIndent(payload, "", "  ")
	if err != nil {
		return ExportResult{}, core.WrapError(core.CodeCheckpoint, "export", "cannot marshal diagnostics", err)
	}
	if err := os.WriteFile(filepath.Join(dir, "diagnostics.json"), append(raw, '\n'), 0o644); err != nil {
		return ExportResult{}, core.WrapError(core.CodeCheckpoint, "export", "cannot write diagnostics", err)
	}
	files++
	return ExportResult{Path: dir, FileCount: files}, nil
}

func parseMode(mode string) (core.RunMode, error) {
	switch strings.ToLower(strings.TrimSpace(mode)) {
	case "realtime":
		return core.RunRealtime, nil
	case "resume":
		return core.RunResume, nil
	case "full":
		return core.RunFull, nil
	default:
		return "", core.NewError(core.CodeParam, "mode", "mode must be realtime, resume, or full")
	}
}

func toAppError(err error) *core.AppError {
	if err == nil {
		return nil
	}
	var appErr *core.AppError
	if ok := asAppError(err, &appErr); ok {
		return appErr
	}
	return core.WrapError(core.CodeState, "", err.Error(), err)
}

func asAppError(err error, target **core.AppError) bool {
	for err != nil {
		if e, ok := err.(*core.AppError); ok {
			*target = e
			return true
		}
		type unwrapper interface{ Unwrap() error }
		u, ok := err.(unwrapper)
		if !ok {
			return false
		}
		err = u.Unwrap()
	}
	return false
}

func dataURL(im core.Image, maxDimension int, mask bool) string {
	scaled := im
	longest := max(im.Width, im.Height)
	if longest > maxDimension {
		factor := (longest + maxDimension - 1) / maxDimension
		if mask {
			scaled = core.UpsampleNearest(core.Downsample(im, factor), max(1, im.Width/factor), max(1, im.Height/factor))
		} else {
			scaled = core.Downsample(im, factor)
		}
	}
	var buf bytes.Buffer
	if err := png.Encode(&buf, core.ToGoImage(scaled)); err != nil {
		return ""
	}
	return "data:image/png;base64," + base64.StdEncoding.EncodeToString(buf.Bytes())
}

func diffHeatmap(preview, exact core.Image) core.Image {
	if preview.Width != exact.Width || preview.Height != exact.Height || preview.Channels != exact.Channels {
		return core.Image{}
	}
	out := core.NewImage(exact.Width, exact.Height, 3)
	for y := 0; y < exact.Height; y++ {
		for x := 0; x < exact.Width; x++ {
			diff := false
			on := false
			for c := 0; c < exact.Channels; c++ {
				if preview.At(x, y, c) != exact.At(x, y, c) {
					diff = true
				}
				if exact.At(x, y, c) != 0 {
					on = true
				}
			}
			var col color.NRGBA
			switch {
			case diff:
				col = color.NRGBA{R: 255, G: 58, B: 34, A: 255}
			case on:
				col = color.NRGBA{R: 22, G: 116, B: 92, A: 255}
			default:
				col = color.NRGBA{R: 8, G: 13, B: 19, A: 255}
			}
			i := (y*exact.Width + x) * 3
			out.Pix[i], out.Pix[i+1], out.Pix[i+2] = col.R, col.G, col.B
		}
	}
	return out
}
