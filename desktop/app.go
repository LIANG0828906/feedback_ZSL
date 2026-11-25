package main

import (
	"context"
	"os"
	"path/filepath"
	"sync"

	core "terrainpipeline"
)

type State struct {
	Workdir string                       `json:"workdir"`
	Outcome *core.Outcome                `json:"outcome"`
	Blocks  map[string]*core.BlockResult `json:"blocks"`
	Report  *core.Report                 `json:"report,omitempty"`
	Message string                       `json:"message,omitempty"`
}

type App struct {
	mu         sync.Mutex
	workdir    string
	project    *core.Project
	store      *core.Store
	service    *core.Service
	outcome    *core.Outcome
	report     *core.Report
	lastEdits  []core.Edit
	failBlocks map[string]string
}

func NewApp() *App {
	workdir := os.Getenv("TERRAIN_STUDIO_HOME")
	if workdir == "" {
		workdir = filepath.Join("artifacts", "wails-studio")
	}
	return &App{workdir: workdir}
}

func (a *App) Bootstrap() (*State, error) {
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.service == nil {
		if err := a.initLocked(); err != nil {
			return nil, err
		}
	}
	return a.stateLocked("ready"), nil
}

func (a *App) GetState() (*State, error) {
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.service == nil {
		return &State{Workdir: a.workdir, Message: "not-bootstrapped"}, nil
	}
	return a.stateLocked("ok"), nil
}

func (a *App) ApplyElevation(blockID string) (*State, error) {
	a.mu.Lock()
	defer a.mu.Unlock()
	if err := a.ensureLocked(); err != nil {
		return nil, err
	}
	block := a.project.BlockByID(blockID)
	if block == nil {
		return nil, core.NewError(core.CodeRef, blockID, "block_id", "块不存在")
	}
	i, j := block.NX/2, block.NZ/2
	block.Elevation[block.Index(i, j)] += 0.075
	a.lastEdits = []core.Edit{{Type: "elevation", BlockID: blockID}}
	outcome, err := a.runLocked(a.lastEdits, false)
	if err != nil {
		return nil, err
	}
	a.outcome = outcome
	return a.stateLocked("elevation edited"), nil
}

func (a *App) ApplyMaterial(layerID string, strength float64) (*State, error) {
	a.mu.Lock()
	defer a.mu.Unlock()
	if err := a.ensureLocked(); err != nil {
		return nil, err
	}
	found := false
	for i := range a.project.Layers {
		if a.project.Layers[i].ID == layerID {
			a.project.Layers[i].Strength = strength
			found = true
		}
	}
	if !found {
		return nil, core.NewError(core.CodeRef, layerID, "layer_id", "材质层不存在")
	}
	a.lastEdits = []core.Edit{{Type: "material", LayerID: layerID}}
	outcome, err := a.runLocked(a.lastEdits, false)
	if err != nil {
		return nil, err
	}
	a.outcome = outcome
	return a.stateLocked("material updated"), nil
}

func (a *App) SetDifferential(operator string) (*State, error) {
	a.mu.Lock()
	defer a.mu.Unlock()
	if err := a.ensureLocked(); err != nil {
		return nil, err
	}
	a.project.Params.Differential = operator
	a.project.ParamVersion = a.project.Params.ParameterVersion()
	a.lastEdits = []core.Edit{{Type: "operator", Detail: operator}}
	outcome, err := a.runLocked(a.lastEdits, false)
	if err != nil {
		return nil, err
	}
	a.outcome = outcome
	return a.stateLocked("operator updated"), nil
}

func (a *App) SimulateBlockFailure(blockID string) (*State, error) {
	a.mu.Lock()
	defer a.mu.Unlock()
	if err := a.ensureLocked(); err != nil {
		return nil, err
	}
	a.failBlocks = map[string]string{blockID: "injected UI block bake failure"}
	block := a.project.BlockByID(blockID)
	if block != nil {
		block.Elevation[block.Index(block.NX/2, block.NZ/2)] += 0.04
	}
	a.lastEdits = []core.Edit{{Type: "elevation", BlockID: blockID}}
	outcome, err := a.runLocked(a.lastEdits, false)
	if err != nil {
		return nil, err
	}
	a.outcome = outcome
	return a.stateLocked("failure simulated"), nil
}

func (a *App) RetryBlock(blockID string) (*State, error) {
	a.mu.Lock()
	defer a.mu.Unlock()
	if err := a.ensureLocked(); err != nil {
		return nil, err
	}
	a.failBlocks = nil
	outcome, err := a.runLocked(a.lastEdits, true)
	if err != nil {
		return nil, err
	}
	a.outcome = outcome
	return a.stateLocked("block retried: " + blockID), nil
}

func (a *App) Export() (string, error) {
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.outcome == nil {
		return "", core.NewError(core.CodeIO, "", "export", "尚无烘焙结果")
	}
	dir := filepath.Join(a.workdir, "export")
	for _, result := range a.outcome.Results {
		if err := core.ExportBlockArtifacts(dir, result); err != nil {
			return "", err
		}
	}
	if err := core.ExportSeams(dir, a.outcome.Seams); err != nil {
		return "", err
	}
	return dir, nil
}

func (a *App) RunVerification() (*core.Report, error) {
	report, err := core.RunVerification(filepath.Join(a.workdir, "verification"))
	if err != nil {
		return nil, err
	}
	a.report = report
	return report, nil
}

func (a *App) initLocked() error {
	if err := os.MkdirAll(a.workdir, 0o755); err != nil {
		return err
	}
	a.project = core.GenerateSample(core.FixedSeed)
	if err := core.WriteSample(filepath.Join(a.workdir, "sample"), a.project); err != nil {
		return err
	}
	store, err := core.OpenStore(filepath.Join(a.workdir, "studio.db"))
	if err != nil {
		return err
	}
	a.store = store
	a.service = core.NewService(filepath.Join(a.workdir, "cache"), store)
	outcome, err := a.runLocked(nil, false)
	if err != nil {
		return err
	}
	a.outcome = outcome
	return nil
}

func (a *App) ensureLocked() error {
	if a.service != nil {
		return nil
	}
	return a.initLocked()
}

func (a *App) runLocked(edits []core.Edit, resume bool) (*core.Outcome, error) {
	opts := core.RunOptions{
		Edits: edits, Resume: resume, FailBlocks: a.failBlocks,
		Budget: core.BudgetOptions{MaxWorkers: 3, PixelBufferPixels: 32, NormalMapMemoryBytes: 4 << 20},
	}
	return a.service.Run(context.Background(), a.project, opts)
}

func (a *App) stateLocked(message string) *State {
	blocks := map[string]*core.BlockResult{}
	if a.outcome != nil {
		for id, result := range a.outcome.Results {
			blocks[id] = result
		}
	}
	return &State{Workdir: a.workdir, Outcome: a.outcome, Blocks: blocks, Report: a.report, Message: message}
}
