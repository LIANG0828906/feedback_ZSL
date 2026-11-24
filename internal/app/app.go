package app

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sync"

	"ocrworkbench/internal/core"
	"ocrworkbench/internal/pipeline"
	"ocrworkbench/internal/sample"
	"ocrworkbench/internal/store"
	"ocrworkbench/internal/verify"
)

type App struct {
	mu       sync.RWMutex
	manifest core.Manifest
	dataDir  string
	dbPath   string
	store    *store.Store
	detector core.DetectorParams
	segment  core.SegmentParams

	running    bool
	paused     bool
	cancel     context.CancelFunc
	control    *pipeline.Control
	lastErr    string
	runs       map[string]core.RunResult
	compare    []verify.Issue
	progress   map[string]core.Progress
	faultKind  string
	faultImage string
	faultIndex int
}

type NewOptions struct {
	DataDir string
	DBPath  string
	Seed    int64
}

func New(opts NewOptions) (*App, error) {
	if opts.DataDir == "" {
		opts.DataDir = "data"
	}
	if opts.DBPath == "" {
		opts.DBPath = "workbench.db"
	}
	manifest, err := sample.LoadManifest(opts.DataDir)
	if err != nil {
		if !os.IsNotExist(err) {
			return nil, err
		}
		manifest, err = sample.Generate(sample.Options{OutputDir: opts.DataDir, Seed: opts.Seed})
		if err != nil {
			return nil, err
		}
	}
	db, err := store.Open(opts.DBPath)
	if err != nil {
		return nil, err
	}
	return &App{
		manifest: manifest,
		dataDir:  opts.DataDir,
		dbPath:   opts.DBPath,
		store:    db,
		detector: core.DefaultDetectorParams(),
		segment:  core.DefaultSegmentParams(),
		runs:     map[string]core.RunResult{},
		progress: map[string]core.Progress{},
	}, nil
}

func (a *App) Close() error {
	if a.store != nil {
		return a.store.Close()
	}
	return nil
}

func (a *App) Manifest() core.Manifest {
	a.mu.RLock()
	defer a.mu.RUnlock()
	return a.manifest
}

func (a *App) DataDir() string {
	a.mu.RLock()
	defer a.mu.RUnlock()
	return a.dataDir
}

func (a *App) Start() error {
	a.mu.Lock()
	if a.running {
		a.mu.Unlock()
		return core.NewError(core.CodeState, "a workbench run is already active")
	}
	ctx, cancel := context.WithCancel(context.Background())
	control := pipeline.NewControl()
	a.running = true
	a.paused = false
	a.cancel = cancel
	a.control = control
	a.lastErr = ""
	a.mu.Unlock()
	go a.runAll(ctx, control)
	return nil
}

func (a *App) Pause() {
	a.mu.Lock()
	control := a.control
	if a.running && control != nil {
		a.paused = true
	}
	a.mu.Unlock()
	if control != nil {
		control.Pause()
	}
}

func (a *App) Resume() {
	a.mu.Lock()
	control := a.control
	if a.running {
		a.paused = false
	}
	a.mu.Unlock()
	if control != nil {
		control.Resume()
	}
}

func (a *App) Cancel() {
	a.mu.Lock()
	control := a.control
	cancel := a.cancel
	a.mu.Unlock()
	if control != nil {
		control.Cancel()
	}
	if cancel != nil {
		cancel()
	}
}

func (a *App) InjectFault(kind, imageID string, index int) error {
	if kind != "tile" && kind != "line" {
		return core.FieldError(core.CodePara, "kind", "[tile,line]", "fault kind must be tile or line")
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.running {
		return core.NewError(core.CodeState, "cannot inject a fault while a run is active")
	}
	a.faultKind = kind
	a.faultImage = imageID
	a.faultIndex = index
	return nil
}

func (a *App) ClearFaults() {
	a.mu.Lock()
	a.faultKind = ""
	a.faultImage = ""
	a.faultIndex = 0
	a.mu.Unlock()
}

func (a *App) runAll(ctx context.Context, control *pipeline.Control) {
	runErr := a.execute(ctx, control)
	a.mu.Lock()
	defer a.mu.Unlock()
	a.running = false
	a.paused = false
	a.control = nil
	if runErr != nil {
		a.lastErr = runErr.Error()
	}
}

func (a *App) execute(ctx context.Context, control *pipeline.Control) error {
	manifest := a.Manifest()
	base := pipeline.Runner{
		Detector: a.detector,
		Segment:  a.segment,
		Control:  control,
	}
	realtimeRunner := base
	realtimeRunner.Store = a.store
	a.mu.RLock()
	faultKind, faultImage, faultIndex := a.faultKind, a.faultImage, a.faultIndex
	a.mu.RUnlock()
	if faultKind == "line" {
		realtimeRunner.Faults.Line = faultMatcher(faultImage, faultIndex)
	} else if faultKind == "tile" {
		realtimeRunner.Faults.Tile = faultMatcher(faultImage, faultIndex)
	}
	realtime, err := realtimeRunner.Run(ctx, manifest, a.dataDir, "realtime")
	if err != nil {
		if errors.Is(err, pipeline.ErrCanceled) || errors.Is(err, context.Canceled) {
			return err
		}
		var unitIssues pipeline.Issues
		if !errors.As(err, &unitIssues) {
			return fmt.Errorf("realtime: %w", err)
		}
		a.mu.Lock()
		a.lastErr = fmt.Sprintf("%d unit(s) failed; inspect the line/tile matrix and retry", len(unitIssues))
		a.mu.Unlock()
	}
	a.setProgress("realtime", realtime)

	resumeDBPath := a.dbPath + ".resume"
	resumeStore, err := store.Open(resumeDBPath)
	if err != nil {
		return err
	}
	defer resumeStore.Close()
	resumeRunner := base
	resumeRunner.Store = resumeStore
	resumeRunner.Faults.CrashAfterTileCommits = 2
	resumeRun, resumeErr := resumeRunner.Run(ctx, manifest, a.dataDir, "resume")
	if errors.Is(resumeErr, pipeline.ErrInjectedCrash) {
		resumeRunner.Faults.CrashAfterTileCommits = 0
		resumeRunner.Faults.CrashAfterLineCommits = 2
		resumeRun, resumeErr = resumeRunner.Run(ctx, manifest, a.dataDir, "resume")
	}
	if errors.Is(resumeErr, pipeline.ErrInjectedCrash) {
		resumeRunner.Faults.CrashAfterLineCommits = 0
		resumeRun, resumeErr = resumeRunner.Run(ctx, manifest, a.dataDir, "resume")
	}
	if resumeErr != nil {
		if errors.Is(resumeErr, pipeline.ErrCanceled) || errors.Is(resumeErr, context.Canceled) {
			return resumeErr
		}
		return fmt.Errorf("resume: %w", resumeErr)
	}
	a.setProgress("resume", resumeRun)

	fullRunner := base
	full, err := fullRunner.Run(ctx, manifest, a.dataDir, "full")
	if err != nil {
		return fmt.Errorf("full: %w", err)
	}
	a.setProgress("full", full)

	var comparison []verify.Issue
	comparison = append(comparison, verify.CompareRuns(realtime, resumeRun)...)
	comparison = append(comparison, verify.CompareRuns(realtime, full)...)
	for _, run := range map[string]core.RunResult{
		"realtime": realtime,
		"resume":   resumeRun,
		"full":     full,
	} {
		comparison = append(comparison, verify.VerifyRun(run, manifest)...)
	}
	exportDir := filepath.Join(a.dataDir, "exports")
	if err := os.MkdirAll(exportDir, 0o755); err != nil {
		return err
	}
	for name, run := range map[string]core.RunResult{"realtime": realtime, "resume": resumeRun, "full": full} {
		if err := verify.WriteRun(filepath.Join(exportDir, name+".json"), run); err != nil {
			return err
		}
	}
	a.mu.Lock()
	a.runs = map[string]core.RunResult{
		"realtime": realtime,
		"resume":   resumeRun,
		"full":     full,
	}
	a.compare = comparison
	if len(comparison) == 0 {
		a.lastErr = ""
	} else {
		a.lastErr = fmt.Sprintf("%d verification issue(s); inspect failed units or retry", len(comparison))
	}
	a.mu.Unlock()
	return nil
}

func faultMatcher(imageID string, index int) func(string, int) error {
	return func(candidateImage string, candidateIndex int) error {
		if (imageID == "" || imageID == "*" || imageID == candidateImage) && (index < 0 || index == candidateIndex) {
			return fmt.Errorf("injected %s failure at index %d", candidateImage, candidateIndex)
		}
		return nil
	}
}

func (a *App) setProgress(kind string, run core.RunResult) {
	progress := core.Progress{}
	for _, image := range run.Images {
		progress.CommittedTiles += image.Progress.CommittedTiles
		progress.TotalTiles += image.Progress.TotalTiles
		progress.CommittedLines += image.Progress.CommittedLines
		progress.TotalLines += image.Progress.TotalLines
	}
	a.mu.Lock()
	a.progress[kind] = progress
	a.mu.Unlock()
}

type Status struct {
	Running        bool                      `json:"running"`
	Paused         bool                      `json:"paused"`
	LastError      string                    `json:"last_error,omitempty"`
	Manifest       core.Manifest             `json:"manifest"`
	Runs           map[string]core.RunResult `json:"runs"`
	Progress       map[string]core.Progress  `json:"progress"`
	Comparison     []verify.Issue            `json:"comparison"`
	VerificationOK bool                      `json:"verification_ok"`
	FaultKind      string                    `json:"fault_kind,omitempty"`
	FaultImage     string                    `json:"fault_image,omitempty"`
	FaultIndex     int                       `json:"fault_index,omitempty"`
}

func (a *App) Status() Status {
	a.mu.RLock()
	defer a.mu.RUnlock()
	runs := make(map[string]core.RunResult, len(a.runs))
	for key, run := range a.runs {
		runs[key] = run
	}
	progress := make(map[string]core.Progress, len(a.progress))
	for key, value := range a.progress {
		progress[key] = value
	}
	return Status{
		Running:        a.running,
		Paused:         a.paused,
		LastError:      a.lastErr,
		Manifest:       a.manifest,
		Runs:           runs,
		Progress:       progress,
		Comparison:     append([]verify.Issue(nil), a.compare...),
		VerificationOK: len(a.compare) == 0 && len(a.runs) == 3,
		FaultKind:      a.faultKind,
		FaultImage:     a.faultImage,
		FaultIndex:     a.faultIndex,
	}
}

func (a *App) Export(kind string) (core.RunResult, bool) {
	a.mu.RLock()
	defer a.mu.RUnlock()
	run, ok := a.runs[kind]
	return run, ok
}

func (a *App) Retry() error {
	a.ClearFaults()
	return a.Start()
}
