//go:build wails

package main

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"time"

	"forensiclab/internal/forensic"
)

type App struct {
	mu           sync.Mutex
	manifestPath string
	imageID      string
	dbPath       string
	reportDir    string
	img          *forensic.Image
	store        *forensic.Store
	engine       *forensic.Engine
	params       forensic.Params
	result       *forensic.RunResult
	comparison   *forensic.Comparison
	ctl          *forensic.Control
	running      bool
	paused       bool
	progress     int
	total        int
	lastError    string
	history      []forensic.RunStep
}

type sessionView struct {
	OK             bool                 `json:"ok"`
	Running        bool                 `json:"running"`
	Paused         bool                 `json:"paused"`
	Progress       int                  `json:"progress"`
	Total          int                  `json:"total"`
	ManifestPath   string               `json:"manifestPath"`
	ImageID        string               `json:"imageId"`
	DBPath         string               `json:"dbPath"`
	ReportDir      string               `json:"reportDir"`
	Params         forensic.Params      `json:"params"`
	Image          *imageView           `json:"image,omitempty"`
	Result         *resultView          `json:"result,omitempty"`
	Comparison     *forensic.Comparison `json:"comparison,omitempty"`
	OriginalBase64 string               `json:"originalBase64,omitempty"`
	HeatmapBase64  string               `json:"heatmapBase64,omitempty"`
	LastError      string               `json:"lastError,omitempty"`
}

type imageView struct {
	ID             string                  `json:"id"`
	Width          int                     `json:"width"`
	Height         int                     `json:"height"`
	Channels       int                     `json:"channels"`
	ManifestPath   string                  `json:"manifestPath"`
	Primitives     []forensic.Primitive    `json:"primitives"`
	TamperRegions  []forensic.TamperRegion `json:"tamperRegions"`
	MaskTolerance  float64                 `json:"maskTolerance"`
	BaselineSHA256 string                  `json:"baselineSha256"`
}

type resultView struct {
	ParamsVersion        string                   `json:"paramsVersion"`
	AlgorithmVersion     string                   `json:"algorithmVersion"`
	BlockSize            int                      `json:"blockSize"`
	Chunks               []forensic.Chunk         `json:"chunks"`
	States               []forensic.BlockState    `json:"states"`
	Summary              forensic.Summary         `json:"summary"`
	Invalidations        []forensic.Invalidation  `json:"invalidations"`
	Reuses               []forensic.ReuseRecord   `json:"reuses"`
	AffectedBlocks       []int                    `json:"affectedBlocks"`
	QueueEvents          []forensic.QueueEvent    `json:"queueEvents"`
	DependencyGraph      forensic.DependencyGraph `json:"dependencyGraph"`
	HeatmapKey           string                   `json:"heatmapKey"`
	HeatmapSHA256        string                   `json:"heatmapSha256"`
	HeatmapReused        bool                     `json:"heatmapReused"`
	SnapshotResetReason  string                   `json:"snapshotResetReason,omitempty"`
	Truth                []forensic.BlockTruth    `json:"truth"`
	TruthConfusion       forensic.TruthConfusion  `json:"truthConfusion"`
	TruthAgreement       float64                  `json:"truthAgreement"`
	TruthWithinTolerance bool                     `json:"truthWithinTolerance"`
	CommitSequence       []int                    `json:"commitSequence"`
	CommitLedger         []int                    `json:"commitLedger"`
	Completed            bool                     `json:"completed"`
}

type runRequest struct {
	Params      *forensic.Params `json:"params,omitempty"`
	ScopeBlocks []int            `json:"scopeBlocks,omitempty"`
}

func NewApp() *App {
	cwd, _ := os.Getwd()
	return &App{
		manifestPath: filepath.Join(cwd, "artifacts", "samples", "manifest.json"),
		dbPath:       filepath.Join(cwd, "artifacts", "workbench.sqlite"),
		reportDir:    filepath.Join(cwd, "artifacts", "reports"),
		params:       forensic.DefaultParams(),
	}
}

func (a *App) startup(ctx context.Context) {
	a.mu.Lock()
	defer a.mu.Unlock()
	if _, err := os.Stat(a.manifestPath); err != nil {
		_, _ = forensic.GenerateSamples(filepath.Dir(a.manifestPath))
	}
	if err := a.openLocked(a.manifestPath, "game_tex_rgb_160x112", a.dbPath); err != nil {
		a.lastError = err.Error()
	}
}

func (a *App) shutdown(ctx context.Context) {
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.ctl != nil {
		a.ctl.Cancel()
	}
	if a.store != nil {
		_ = a.store.Close()
		a.store = nil
	}
}

func (a *App) GenerateSamples(dir string) (string, error) {
	if dir == "" {
		dir = filepath.Dir(a.manifestPath)
	}
	generated, err := forensic.GenerateSamples(dir)
	if err != nil {
		return "", err
	}
	return jsonString(map[string]any{"manifest": generated.Path, "images": generated.Manifest.Images})
}

func (a *App) ListSamples(manifestPath string) (string, error) {
	if manifestPath == "" {
		a.mu.Lock()
		manifestPath = a.manifestPath
		a.mu.Unlock()
	}
	m, err := forensic.LoadManifest(manifestPath)
	if err != nil {
		return "", err
	}
	return jsonString(map[string]any{"manifestPath": manifestPath, "images": m.Images})
}

func (a *App) OpenWorkspace(manifestPath, imageID, dbPath string) (string, error) {
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.running {
		return "", forensic.ParamError("running", "cancel the active analysis before opening another workspace")
	}
	if err := a.openLocked(manifestPath, imageID, dbPath); err != nil {
		return "", err
	}
	return a.sessionJSONLocked()
}

func (a *App) openLocked(manifestPath, imageID, dbPath string) error {
	if imageID == "" {
		imageID = "game_tex_rgb_160x112"
	}
	if dbPath == "" {
		dbPath = a.dbPath
	}
	img, err := forensic.LoadSample(manifestPath, imageID)
	if err != nil {
		return err
	}
	if a.store != nil {
		_ = a.store.Close()
	}
	store, err := forensic.OpenStore(dbPath)
	if err != nil {
		return err
	}
	a.manifestPath, a.imageID, a.dbPath = manifestPath, imageID, dbPath
	a.img, a.store = img, store
	a.engine = forensic.NewEngine(img, store)
	a.result = nil
	a.comparison = nil
	a.ctl = nil
	a.running = false
	a.paused = false
	a.progress, a.total = 0, 0
	a.lastError = ""
	a.history = nil
	return nil
}

func (a *App) SetParamsJSON(input string) (string, error) {
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.running {
		return "", forensic.ParamError("running", "parameters cannot be changed while analysis is running")
	}
	var p forensic.Params
	if err := json.Unmarshal([]byte(input), &p); err != nil {
		return "", forensic.DecodeError("decode parameters", err)
	}
	if a.img != nil {
		if err := p.Validate(a.img); err != nil {
			return "", err
		}
	}
	a.params = p
	return a.sessionJSONLocked()
}

func (a *App) RunJSON(input string) (string, error) {
	var req runRequest
	if input != "" {
		if err := json.Unmarshal([]byte(input), &req); err != nil {
			return "", forensic.DecodeError("decode run request", err)
		}
	}
	a.mu.Lock()
	if a.running {
		a.mu.Unlock()
		return "", forensic.ParamError("running", "an analysis is already running")
	}
	if a.engine == nil || a.img == nil {
		a.mu.Unlock()
		return "", forensic.ParamError("workspace", "no workspace is open")
	}
	if req.Params != nil {
		if err := req.Params.Validate(a.img); err != nil {
			a.mu.Unlock()
			return "", err
		}
		a.params = *req.Params
	}
	p := a.params
	engine := a.engine
	ctl := forensic.NewControl()
	a.ctl = ctl
	a.running = true
	a.paused = false
	a.progress = 0
	chunks, _ := forensic.MakeChunks(a.img.Width, a.img.Height, p.BlockSize)
	a.total = len(chunks)
	a.lastError = ""
	a.mu.Unlock()

	opts := forensic.RunOptions{
		ScopeBlocks: req.ScopeBlocks,
		CommitHook: func(state forensic.BlockState) error {
			a.mu.Lock()
			a.progress++
			a.mu.Unlock()
			return nil
		},
	}
	result, err := engine.Run(context.Background(), p, opts, ctl)

	a.mu.Lock()
	a.running = false
	a.paused = false
	if err == nil {
		copy := result
		a.result = &copy
		a.progress = len(result.States)
		a.total = len(result.Chunks)
		a.history = append(a.history, forensic.RunStep{Params: p, Scope: append([]int(nil), req.ScopeBlocks...)})
	} else {
		a.lastError = err.Error()
		if len(result.States) > 0 {
			copy := result
			a.result = &copy
		}
	}
	a.mu.Unlock()
	if err != nil && !errors.Is(err, forensic.ErrCancelled) {
		return "", err
	}
	return a.GetSession()
}

func (a *App) Pause() string {
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.ctl != nil && a.running {
		a.ctl.Pause()
		a.paused = true
	}
	return "paused"
}

func (a *App) Resume() string {
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.ctl != nil && a.running {
		a.ctl.Resume()
		a.paused = false
	}
	return "running"
}

func (a *App) Cancel() string {
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.ctl != nil && a.running {
		a.ctl.Cancel()
	}
	return "cancel-requested"
}

func (a *App) RetryBlock(index int) (string, error) {
	payload, _ := json.Marshal(runRequest{ScopeBlocks: []int{index}})
	return a.RunJSON(string(payload))
}

func (a *App) ResetCheckpoint() (string, error) {
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.running {
		return "", forensic.ParamError("running", "cancel the active analysis before resetting the checkpoint")
	}
	if a.store != nil {
		_ = a.store.Close()
	}
	base := a.dbPath
	for _, suffix := range []string{"", "-wal", "-shm"} {
		_ = os.Remove(base + suffix)
	}
	store, err := forensic.OpenStore(base)
	if err != nil {
		return "", err
	}
	a.store = store
	a.engine = forensic.NewEngine(a.img, store)
	a.result = nil
	a.comparison = nil
	a.history = nil
	a.lastError = ""
	return a.sessionJSONLocked()
}

func (a *App) VerifyPersistedHashes() (string, error) {
	a.mu.Lock()
	engine := a.engine
	a.mu.Unlock()
	if engine == nil {
		return "", forensic.ParamError("workspace", "no workspace is open")
	}
	if err := engine.VerifyPersistedHashes(context.Background()); err != nil {
		return "", err
	}
	return jsonString(map[string]any{"ok": true, "message": "all persisted block content hashes match the loaded image"})
}

func (a *App) VerifyThreeWay() (string, error) {
	a.mu.Lock()
	if a.running {
		a.mu.Unlock()
		return "", forensic.ParamError("running", "wait for or cancel the active analysis")
	}
	img, current := a.img, a.params
	history := append([]forensic.RunStep(nil), a.history...)
	root := filepath.Dir(a.dbPath)
	a.mu.Unlock()
	if img == nil {
		return "", forensic.ParamError("workspace", "no workspace is open")
	}
	base := forensic.DefaultParams()
	base.BlockSize, base.WorkerCount = current.BlockSize, current.WorkerCount
	base.PixelBufferLimit, base.QueueCapacity = current.PixelBufferLimit, current.QueueCapacity
	base.HashAlgorithm, base.FingerprintGrid = current.HashAlgorithm, current.FingerprintGrid
	base.Window = current.Window
	base.Threshold = 0.70
	steps := history
	if len(steps) == 0 {
		steps = []forensic.RunStep{{Params: base}, {Params: current}}
	} else {
		if len(steps) == 1 && forensic.ParamsVersion(img, steps[0].Params) != forensic.ParamsVersion(img, base) {
			steps = append([]forensic.RunStep{{Params: base}}, steps...)
		}
		if forensic.ParamsVersion(img, steps[len(steps)-1].Params) != forensic.ParamsVersion(img, current) {
			steps = append(steps, forensic.RunStep{Params: current})
		}
	}
	workdir := filepath.Join(root, "verification", fmt.Sprintf("%d", time.Now().UnixNano()))
	report, err := forensic.VerifyThreePaths(context.Background(), img, workdir, steps)
	if err != nil {
		if report != nil {
			a.mu.Lock()
			a.comparison = &report.Comparison
			a.mu.Unlock()
		}
		return "", err
	}
	a.mu.Lock()
	a.comparison = &report.Comparison
	a.mu.Unlock()
	return a.sessionJSON()
}

func (a *App) ExportReport(dir string) (string, error) {
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.result == nil || a.img == nil {
		return "", forensic.ParamError("result", "run an analysis before exporting a report")
	}
	if dir == "" {
		dir = a.reportDir
	}
	files, err := forensic.WriteReport(dir, a.img, a.result, a.comparison)
	if err != nil {
		return "", err
	}
	return jsonString(files)
}

func (a *App) GetSession() (string, error) {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.sessionJSONLocked()
}

func (a *App) sessionJSON() (string, error) {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.sessionJSONLocked()
}

func (a *App) sessionJSONLocked() (string, error) {
	view := sessionView{
		OK: a.lastError == "", Running: a.running, Paused: a.paused,
		Progress: a.progress, Total: a.total, ManifestPath: a.manifestPath,
		ImageID: a.imageID, DBPath: a.dbPath, ReportDir: a.reportDir,
		Params: a.params, Comparison: a.comparison, LastError: a.lastError,
	}
	if a.img != nil {
		view.Image = &imageView{
			ID: a.img.ID, Width: a.img.Width, Height: a.img.Height, Channels: a.img.Channels,
			ManifestPath: a.img.ManifestPath, Primitives: a.img.Entry.Primitives,
			TamperRegions: a.img.Entry.TamperRegions, MaskTolerance: a.img.Entry.MaskTolerance,
			BaselineSHA256: a.img.Entry.BaselineSHA256,
		}
		view.OriginalBase64 = base64.StdEncoding.EncodeToString(a.img.ImagePNG)
	}
	if a.result != nil {
		view.Result = &resultView{
			ParamsVersion: a.result.ParamsVersion, AlgorithmVersion: a.result.AlgorithmVersion,
			BlockSize: a.result.BlockSize, Chunks: a.result.Chunks, States: a.result.States,
			Summary: a.result.Summary, Invalidations: a.result.Invalidations, Reuses: a.result.Reuses,
			AffectedBlocks: a.result.AffectedBlocks, QueueEvents: a.result.QueueEvents,
			DependencyGraph: a.result.DependencyGraph, HeatmapKey: a.result.HeatmapKey,
			HeatmapSHA256: a.result.HeatmapSHA256, HeatmapReused: a.result.HeatmapReused,
			SnapshotResetReason: a.result.SnapshotResetReason, Truth: a.result.Truth,
			TruthConfusion: a.result.TruthConfusion, TruthAgreement: a.result.TruthAgreement,
			TruthWithinTolerance: a.result.TruthWithinTolerance,
			CommitSequence:       a.result.CommitSequence, CommitLedger: a.result.CommitLedger,
			Completed: a.result.Completed,
		}
		view.HeatmapBase64 = base64.StdEncoding.EncodeToString(a.result.HeatmapPNG)
	}
	return jsonString(view)
}

func jsonString(v any) (string, error) {
	data, err := json.Marshal(v)
	if err != nil {
		return "", err
	}
	return string(data), nil
}
