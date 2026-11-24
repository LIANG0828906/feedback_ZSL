package main

import (
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"sync"

	"terrainnormalverify/internal/checkpoint"
	"terrainnormalverify/internal/dependency"
	"terrainnormalverify/internal/engine"
	"terrainnormalverify/internal/exportpng"
	"terrainnormalverify/internal/model"
	"terrainnormalverify/internal/projectio"
	"terrainnormalverify/internal/seam"
)

type App struct {
	mu          sync.Mutex
	projectPath string
	dbPath      string
	exportDir   string
	budget      engine.Budget
	project     *model.Project
	store       *checkpoint.Store
	runner      *engine.Engine
	lastReport  *engine.RunReport
}

type UIState struct {
	Project       UIProject                      `json:"project"`
	Blocks        []UIBlock                      `json:"blocks"`
	Seams         []seam.SeamReport              `json:"seams"`
	Invalidation  []dependency.InvalidationEntry `json:"invalidation"`
	LastReport    *engine.RunReport              `json:"lastReport,omitempty"`
	Budget        engine.Budget                  `json:"budget"`
	PeakMemory    int64                          `json:"peakMemoryBytes"`
	Queue         []engine.QueueRecord           `json:"queue"`
	ExportPaths   map[string]string              `json:"exportPaths,omitempty"`
	StatusMessage string                         `json:"statusMessage"`
}

type UIProject struct {
	Path             string `json:"path"`
	DatabasePath     string `json:"databasePath"`
	Revision         int64  `json:"revision"`
	ParameterVersion string `json:"parameterVersion"`
	AlgorithmVersion string `json:"algorithmVersion"`
	Seed             int64  `json:"seed"`
	BlockCount       int    `json:"blockCount"`
	SeamCount        int    `json:"seamCount"`
}

type UIBlock struct {
	ID          int       `json:"id"`
	Name        string    `json:"name"`
	Width       int       `json:"width"`
	Height      int       `json:"height"`
	OriginX     int       `json:"originX"`
	OriginY     int       `json:"originY"`
	Colors      []int     `json:"colors"`
	Slopes      []float64 `json:"slopes"`
	ContentHash string    `json:"contentHash,omitempty"`
	NormalHash  string    `json:"normalHash,omitempty"`
	SlopeHash   string    `json:"slopeHash,omitempty"`
	Status      string    `json:"status"`
	BakeCount   int       `json:"bakeCount"`
	RebakeMS    float64   `json:"rebakeMs"`
	Failed      bool      `json:"failed"`
}

func NewApp() *App {
	projectPath := os.Getenv("TERRAIN_PROJECT")
	if projectPath == "" {
		projectPath = filepath.Join("samples", "terrain_project.json")
	}
	dbPath := os.Getenv("TERRAIN_DB")
	if dbPath == "" {
		dbPath = filepath.Join("artifacts", "wails-diagnostics.sqlite")
	}
	return &App{
		projectPath: projectPath,
		dbPath:      dbPath,
		exportDir:   filepath.Join("artifacts", "wails-export"),
		budget:      engine.DefaultBudget(),
	}
}

func (a *App) Initialize() (*UIState, error) {
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.runner != nil {
		return a.stateLocked(), nil
	}
	project, err := projectio.Load(a.projectPath)
	if err != nil {
		return nil, err
	}
	store, err := checkpoint.Open(a.dbPath)
	if err != nil {
		return nil, err
	}
	runner, err := engine.New(project, store, engine.Config{Budget: a.budget})
	if err != nil {
		store.Close()
		return nil, err
	}
	report, err := runner.Initialize()
	a.lastReport = report
	if err != nil {
		store.Close()
		return nil, err
	}
	a.project = runner.Project()
	a.store = store
	a.runner = runner
	return a.stateLocked(), nil
}

func (a *App) GetState() (*UIState, error) {
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.runner == nil {
		return a.initializeLocked()
	}
	return a.stateLocked(), nil
}

func (a *App) EditHeightDelta(blockID, x, y int, delta float64) (*UIState, error) {
	return a.applyEdit(engine.Edit{Kind: "height_delta", BlockID: blockID, X: x, Y: y, Value: delta})
}

func (a *App) SetHeight(blockID, x, y int, value float64) (*UIState, error) {
	return a.applyEdit(engine.Edit{Kind: "height_set", BlockID: blockID, X: x, Y: y, Value: value})
}

func (a *App) EditMaterial(layerID string, biasX, biasY, biasZ, strength float64) (*UIState, error) {
	return a.applyEdit(engine.Edit{
		Kind: "layer_bias", LayerID: layerID,
		BiasX: biasX, BiasY: biasY, BiasZ: biasZ, Strength: strength,
	})
}

func (a *App) SetDifferenceOperator(operator string) (*UIState, error) {
	return a.applyEdit(engine.Edit{Kind: "difference_operator", Operator: operator})
}

func (a *App) SetScale(scale float64) (*UIState, error) {
	return a.applyEdit(engine.Edit{Kind: "scale", Value: scale})
}

func (a *App) RetryFailedBlocks() (*UIState, error) {
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.runner == nil {
		return a.initializeLocked()
	}
	report, err := a.runner.RetryFailed()
	a.lastReport = report
	if err != nil {
		return a.stateLocked(), err
	}
	a.project = a.runner.Project()
	if err := projectio.Save(a.projectPath, a.project); err != nil {
		return a.stateLocked(), err
	}
	return a.stateLocked(), nil
}

func (a *App) RetryBlock(blockID int) (*UIState, error) {
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.runner == nil {
		return a.initializeLocked()
	}
	report, err := a.runner.RetryBlock(blockID)
	a.lastReport = report
	if err != nil {
		return a.stateLocked(), err
	}
	a.project = a.runner.Project()
	if err := projectio.Save(a.projectPath, a.project); err != nil {
		return a.stateLocked(), err
	}
	return a.stateLocked(), nil
}

func (a *App) RebuildAll() (*UIState, error) {
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.runner == nil {
		return a.initializeLocked()
	}
	report, err := a.runner.BakeAll("visual diagnostic full rebuild")
	a.lastReport = report
	if err != nil {
		return a.stateLocked(), err
	}
	a.project = a.runner.Project()
	return a.stateLocked(), nil
}

func (a *App) ExportNormals() (*UIState, error) {
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.runner == nil {
		return a.initializeLocked()
	}
	paths, err := exportpng.Write(a.project, a.runner.Snapshot(), a.exportDir)
	if err != nil {
		return a.stateLocked(), err
	}
	state := a.stateLocked()
	state.ExportPaths = paths
	state.StatusMessage = "normal-map.png and slope-heatmap.png exported"
	return state, nil
}

func (a *App) applyEdit(edit engine.Edit) (*UIState, error) {
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.runner == nil {
		if _, err := a.initializeLocked(); err != nil {
			return nil, err
		}
	}
	report, err := a.runner.ApplyEdits([]engine.Edit{edit})
	a.lastReport = report
	if err != nil {
		return a.stateLocked(), err
	}
	a.project = a.runner.Project()
	if err := projectio.Save(a.projectPath, a.project); err != nil {
		return a.stateLocked(), err
	}
	return a.stateLocked(), nil
}

func (a *App) initializeLocked() (*UIState, error) {
	project, err := projectio.Load(a.projectPath)
	if err != nil {
		return nil, err
	}
	store, err := checkpoint.Open(a.dbPath)
	if err != nil {
		return nil, err
	}
	runner, err := engine.New(project, store, engine.Config{Budget: a.budget})
	if err != nil {
		store.Close()
		return nil, err
	}
	report, err := runner.Initialize()
	a.lastReport = report
	if err != nil {
		store.Close()
		return nil, err
	}
	a.project = runner.Project()
	a.store = store
	a.runner = runner
	return a.stateLocked(), nil
}

func (a *App) stateLocked() *UIState {
	state := &UIState{
		Project: UIProject{
			Path:             a.projectPath,
			DatabasePath:     a.dbPath,
			Seed:             a.project.Seed,
			BlockCount:       len(a.project.Blocks),
			SeamCount:        len(a.project.Seams),
			AlgorithmVersion: model.AlgorithmVersion,
		},
		Budget:        a.budget,
		LastReport:    a.lastReport,
		StatusMessage: "ready",
	}
	if a.runner != nil {
		snapshot := a.runner.Snapshot()
		project := a.runner.Project()
		state.Project.Revision = project.Revision
		state.Project.ParameterVersion = snapshot.ParameterVersion
		state.Seams = snapshot.Seams
	}
	if a.lastReport != nil {
		state.Invalidation = a.lastReport.Invalidation
		state.Queue = a.lastReport.Queue
		state.PeakMemory = a.lastReport.PeakMemoryBytes
	}
	stats := make(map[int]engine.BlockStat)
	if a.lastReport != nil {
		for _, stat := range a.lastReport.Blocks {
			stats[stat.BlockID] = stat
		}
	}
	if a.runner == nil {
		return state
	}
	snapshot := a.runner.Snapshot()
	for _, block := range a.project.SortedBlocks() {
		result := snapshot.Blocks[block.ID]
		if result == nil {
			continue
		}
		stat := stats[block.ID]
		status := "cache-hit"
		if stat.Rebaked {
			status = "rebaked"
		}
		if stat.Failure != "" {
			status = "failed"
		}
		colors := make([]int, len(result.Colors))
		for i, value := range result.Colors {
			colors[i] = int(value)
		}
		state.Blocks = append(state.Blocks, UIBlock{
			ID:          block.ID,
			Name:        block.Name,
			Width:       result.Width,
			Height:      result.Height,
			OriginX:     result.OriginX,
			OriginY:     result.OriginY,
			Colors:      colors,
			Slopes:      append([]float64(nil), result.Slopes...),
			ContentHash: stat.ContentHash,
			NormalHash:  stat.NormalHash,
			SlopeHash:   stat.SlopeHash,
			Status:      status,
			BakeCount:   stat.BakeCount,
			RebakeMS:    stat.DurationMS,
			Failed:      stat.Failure != "",
		})
	}
	sort.Slice(state.Blocks, func(i, j int) bool { return state.Blocks[i].ID < state.Blocks[j].ID })
	return state
}

func (a *App) Close() error {
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.store == nil {
		return nil
	}
	err := a.store.Close()
	a.store = nil
	a.runner = nil
	if err != nil {
		return model.NewError(model.ErrIO, -1, "close", err.Error())
	}
	return nil
}

func (a *App) Describe() string {
	return fmt.Sprintf("project=%s db=%s workers=%d buffer=%d memory=%d",
		a.projectPath, a.dbPath, a.budget.MaxParallelism, a.budget.PerThreadPixelBuffer, a.budget.NormalMapMemoryBytes)
}
