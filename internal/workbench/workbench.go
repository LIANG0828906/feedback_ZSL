package workbench

import (
	"context"
	"os"
	"path/filepath"
	"sync"

	"volumediag/internal/diag"
	"volumediag/internal/engine"
	"volumediag/internal/model"
	"volumediag/internal/scene"
	"volumediag/internal/volume"
)

type ActionResult struct {
	OK    bool        `json:"ok"`
	Error *diag.Error `json:"error,omitempty"`
	View  *View       `json:"view,omitempty"`
}

type View struct {
	State       engine.State                 `json:"state"`
	Image       engine.ImageData             `json:"image"`
	Coverage    map[string][]volume.BlockKey `json:"coverage"`
	ScenePath   string                       `json:"scene_path"`
	DBPath      string                       `json:"db_path"`
	LastReport  *engine.RenderReport         `json:"last_report,omitempty"`
	LastEdit    *engine.EditReport           `json:"last_edit,omitempty"`
	LastCompare *engine.CompareReport        `json:"last_compare,omitempty"`
	LastTruth   *engine.TruthReport          `json:"last_truth,omitempty"`
	Message     string                       `json:"message,omitempty"`
}

type BlockDetail struct {
	Found         bool                 `json:"found"`
	View          engine.BlockView     `json:"view"`
	Width         int                  `json:"width"`
	Height        int                  `json:"height"`
	Stats         volume.BlockStats    `json:"stats"`
	Pixels        []volume.PixelResult `json:"pixels"`
	Composite     []model.Color        `json:"composite"`
	UsedOccluders []string             `json:"used_occluders"`
}

type Config struct {
	Step                float64 `json:"step"`
	Jitter              string  `json:"jitter"`
	JitterSamples       int     `json:"jitter_samples"`
	MaxWorkers          int     `json:"max_workers"`
	MaxSamplesPerThread int     `json:"max_samples_per_thread"`
	BufferSlots         int     `json:"buffer_slots"`
	BatchSize           int     `json:"batch_size"`
}

type Workbench struct {
	ctx context.Context

	opMu sync.Mutex
	mu   sync.RWMutex

	eng         *engine.Engine
	sceneDir    string
	dbPath      string
	lastEdit    *engine.EditReport
	lastCompare *engine.CompareReport
	lastTruth   *engine.TruthReport
	lastErr     *diag.Error
}

func New() *Workbench {
	return &Workbench{ctx: context.Background()}
}

func (w *Workbench) SetContext(ctx context.Context) {
	w.mu.Lock()
	w.ctx = ctx
	w.mu.Unlock()
}

func (w *Workbench) Bootstrap(sceneDir, dbPath string, seed int64) ActionResult {
	w.opMu.Lock()
	defer w.opMu.Unlock()
	if sceneDir == "" {
		sceneDir = filepath.Join(defaultBaseDir(), "gui-sample")
	}
	if dbPath == "" {
		dbPath = filepath.Join(defaultBaseDir(), "gui-checkpoint.db")
	}
	if _, err := os.Stat(filepath.Join(sceneDir, "scene.json")); err != nil {
		if !os.IsNotExist(err) {
			return w.failure(diag.IO("inspect scene directory", err))
		}
		opts := scene.DefaultGenerateOptions()
		if seed != 0 {
			opts.Seed = seed
		}
		if _, err := scene.Generate(sceneDir, opts); err != nil {
			return w.failure(err)
		}
	}
	bundle, err := model.LoadBundle(sceneDir)
	if err != nil {
		return w.failure(err)
	}
	params := volume.DefaultParams()
	params.CameraID = bundle.Scene.ActiveCameraID
	next, err := engine.Open(bundle, params, dbPath)
	if err != nil {
		return w.failure(err)
	}
	ctx := w.context()
	report, err := next.Render(ctx, engine.ModeResume, engine.RenderOptions{})
	if err != nil {
		_ = next.Close()
		return w.failure(err)
	}
	w.mu.Lock()
	old := w.eng
	w.eng = next
	w.sceneDir = sceneDir
	w.dbPath = dbPath
	w.lastEdit = nil
	w.lastCompare = nil
	w.lastTruth = nil
	w.lastErr = nil
	w.mu.Unlock()
	if old != nil {
		_ = old.Close()
	}
	return w.success("loaded deterministic sample and resumed checkpoint", report)
}

func (w *Workbench) GenerateSample(outDir string, seed int64) ActionResult {
	if outDir == "" {
		outDir = filepath.Join("artifacts", "gui-sample")
	}
	opts := scene.DefaultGenerateOptions()
	if seed != 0 {
		opts.Seed = seed
	}
	_, err := scene.Generate(outDir, opts)
	if err != nil {
		return w.failure(err)
	}
	return ActionResult{OK: true}
}

func (w *Workbench) RenderFull() ActionResult {
	return w.render(engine.ModeFull, "full rebuild")
}

func (w *Workbench) RenderPreview() ActionResult {
	w.opMu.Lock()
	defer w.opMu.Unlock()
	eng, err := w.engine()
	if err != nil {
		return w.failure(err)
	}
	base := eng.Params()
	p := volume.PreviewParams()
	p.MaxWorkers = base.MaxWorkers
	p.MaxSamplesPerThread = base.MaxSamplesPerThread
	p.BufferSlots = base.BufferSlots
	p.BatchSize = base.BatchSize
	p.CameraID = base.CameraID
	if err := eng.SetParams(p); err != nil {
		return w.failure(err)
	}
	w.clearVerification()
	report, err := eng.Render(w.context(), engine.ModePreview, engine.RenderOptions{})
	if err != nil {
		return w.failure(err)
	}
	return w.success("preview rendered; quality bound is derived from observed step/sample count", report)
}

func (w *Workbench) RenderResume() ActionResult {
	return w.render(engine.ModeResume, "checkpoint resumed")
}

func (w *Workbench) render(mode engine.Mode, message string) ActionResult {
	w.opMu.Lock()
	defer w.opMu.Unlock()
	eng, err := w.engine()
	if err != nil {
		return w.failure(err)
	}
	w.clearVerification()
	report, err := eng.Render(w.context(), mode, engine.RenderOptions{})
	if err != nil {
		return w.failure(err)
	}
	return w.success(message, report)
}

func (w *Workbench) ApplyEdit(req engine.EditRequest) ActionResult {
	w.opMu.Lock()
	defer w.opMu.Unlock()
	eng, err := w.engine()
	if err != nil {
		return w.failure(err)
	}
	edit, err := eng.ApplyEdit(req)
	if err != nil {
		return w.failure(err)
	}
	w.mu.Lock()
	w.lastEdit = edit
	w.lastCompare = nil
	w.lastTruth = nil
	w.mu.Unlock()
	report, err := eng.Render(w.context(), engine.ModeIncremental, engine.RenderOptions{})
	if err != nil {
		return w.failure(err)
	}
	return w.success("edit applied; only invalidated blocks were rebuilt", report)
}

func (w *Workbench) Configure(cfg Config) ActionResult {
	w.opMu.Lock()
	defer w.opMu.Unlock()
	eng, err := w.engine()
	if err != nil {
		return w.failure(err)
	}
	before := eng.Params().Version()
	p := eng.Params()
	p.Step = cfg.Step
	p.Jitter = volume.JitterMode(cfg.Jitter)
	p.JitterSamples = cfg.JitterSamples
	p.MaxWorkers = cfg.MaxWorkers
	p.MaxSamplesPerThread = cfg.MaxSamplesPerThread
	p.BufferSlots = cfg.BufferSlots
	p.BatchSize = cfg.BatchSize
	if err := eng.SetParams(p); err != nil {
		return w.failure(err)
	}
	if before != p.Version() {
		w.clearVerification()
		return w.success("render parameters changed; all blocks are marked invalidated", nil)
	}
	return w.success("resource limits updated; committed blocks remain valid", nil)
}

func (w *Workbench) Pause() ActionResult {
	eng, err := w.engine()
	if err != nil {
		return w.failure(err)
	}
	eng.Pause()
	return w.success("pause requested; the current batch will finish before stopping", nil)
}

func (w *Workbench) ResumeRendering() ActionResult {
	eng, err := w.engine()
	if err != nil {
		return w.failure(err)
	}
	eng.Resume()
	return w.success("pause flag cleared; click resume-render to continue", nil)
}

func (w *Workbench) Compare() ActionResult {
	w.opMu.Lock()
	defer w.opMu.Unlock()
	eng, err := w.engine()
	if err != nil {
		return w.failure(err)
	}
	cmp, err := eng.VerifyAgainstFull(w.context())
	w.mu.Lock()
	w.lastCompare = cmp
	w.mu.Unlock()
	if err != nil {
		return w.failure(err)
	}
	return w.success("incremental cache equals an empty-cache full rebuild", nil)
}

func (w *Workbench) VerifyTruth() ActionResult {
	w.opMu.Lock()
	defer w.opMu.Unlock()
	eng, err := w.engine()
	if err != nil {
		return w.failure(err)
	}
	truth, err := eng.VerifyTruth(w.context())
	w.mu.Lock()
	w.lastTruth = truth
	w.mu.Unlock()
	if err != nil {
		return w.failure(err)
	}
	return w.success("rendered pixels and probe rays are inside the declared truth tolerance", nil)
}

func (w *Workbench) RetryFailed() ActionResult {
	w.opMu.Lock()
	defer w.opMu.Unlock()
	eng, err := w.engine()
	if err != nil {
		return w.failure(err)
	}
	w.clearVerification()
	report, err := eng.Retry(w.context())
	if err != nil {
		return w.failure(err)
	}
	return w.success("failed blocks retried; successful batches remain committed", report)
}

func (w *Workbench) State() ActionResult {
	return w.success("", nil)
}

func (w *Workbench) Block(x, y int) BlockDetail {
	eng, err := w.engine()
	if err != nil {
		return BlockDetail{}
	}
	view, block, ok := eng.Block(x, y)
	if !ok {
		return BlockDetail{}
	}
	return BlockDetail{
		Found:         true,
		View:          view,
		Width:         block.Width,
		Height:        block.Height,
		Stats:         block.Stats,
		Pixels:        block.Pixels,
		Composite:     block.Composite,
		UsedOccluders: block.UsedOccluders,
	}
}

func (w *Workbench) LightCoverage(lightID string) []volume.BlockKey {
	eng, err := w.engine()
	if err != nil {
		return nil
	}
	return eng.LightCoverageBlocks(lightID)
}

func (w *Workbench) success(message string, report *engine.RenderReport) ActionResult {
	w.mu.RLock()
	eng := w.eng
	lastEdit := w.lastEdit
	lastCompare := w.lastCompare
	lastTruth := w.lastTruth
	sceneDir := w.sceneDir
	dbPath := w.dbPath
	w.mu.RUnlock()
	if eng == nil {
		return w.failure(diag.New(diag.ERef, "no scene is loaded"))
	}
	coverage := make(map[string][]volume.BlockKey)
	state := eng.State()
	if report == nil {
		report = eng.LastReport()
	}
	for i := range state.Lights {
		coverage[state.Lights[i].ID] = eng.LightCoverageBlocks(state.Lights[i].ID)
	}
	view := &View{
		State:       state,
		Image:       eng.Image(),
		Coverage:    coverage,
		ScenePath:   sceneDir,
		DBPath:      dbPath,
		LastReport:  report,
		LastEdit:    lastEdit,
		LastCompare: lastCompare,
		LastTruth:   lastTruth,
		Message:     message,
	}
	return ActionResult{OK: true, View: view}
}

func (w *Workbench) failure(err error) ActionResult {
	de := diag.As(err)
	if de == nil {
		de = &diag.Error{Code: diag.EIO, Message: err.Error()}
	}
	w.mu.Lock()
	w.lastErr = de
	w.mu.Unlock()
	action := &ActionResult{OK: false, Error: de}
	w.mu.RLock()
	if eng := w.eng; eng != nil {
		coverage := make(map[string][]volume.BlockKey)
		state := eng.State()
		for i := range state.Lights {
			coverage[state.Lights[i].ID] = eng.LightCoverageBlocks(state.Lights[i].ID)
		}
		action.View = &View{
			State:       state,
			Image:       eng.Image(),
			Coverage:    coverage,
			ScenePath:   w.sceneDir,
			DBPath:      w.dbPath,
			LastEdit:    w.lastEdit,
			LastCompare: w.lastCompare,
			LastTruth:   w.lastTruth,
			Message:     de.Error(),
		}
	}
	w.mu.RUnlock()
	return *action
}

func (w *Workbench) engine() (*engine.Engine, error) {
	w.mu.RLock()
	defer w.mu.RUnlock()
	if w.eng == nil {
		return nil, diag.New(diag.ERef, "no scene is loaded")
	}
	return w.eng, nil
}

func (w *Workbench) context() context.Context {
	w.mu.RLock()
	defer w.mu.RUnlock()
	if w.ctx == nil {
		return context.Background()
	}
	return w.ctx
}

func (w *Workbench) clearVerification() {
	w.mu.Lock()
	w.lastCompare = nil
	w.lastTruth = nil
	w.mu.Unlock()
}

func (w *Workbench) ScenePath() string {
	w.mu.RLock()
	defer w.mu.RUnlock()
	return w.sceneDir
}

func (w *Workbench) DBPath() string {
	w.mu.RLock()
	defer w.mu.RUnlock()
	return w.dbPath
}

func (w *Workbench) Close() error {
	w.opMu.Lock()
	defer w.opMu.Unlock()
	w.mu.Lock()
	eng := w.eng
	w.eng = nil
	w.mu.Unlock()
	if eng != nil {
		return eng.Close()
	}
	return nil
}

func defaultBaseDir() string {
	candidates := make([]string, 0, 4)
	if wd, err := os.Getwd(); err == nil {
		candidates = append(candidates, wd)
	}
	if exe, err := os.Executable(); err == nil {
		candidates = append(candidates, filepath.Dir(exe))
	}
	for _, start := range candidates {
		dir := start
		for {
			if _, err := os.Stat(filepath.Join(dir, "go.mod")); err == nil {
				return filepath.Join(dir, "artifacts")
			}
			parent := filepath.Dir(dir)
			if parent == dir {
				break
			}
			dir = parent
		}
	}
	if cache, err := os.UserCacheDir(); err == nil {
		return filepath.Join(cache, "volumediag")
	}
	return filepath.Join("artifacts")
}
