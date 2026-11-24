//go:build wails

package main

import (
	"context"
	_ "embed"
	"encoding/json"
	"os"
	"path/filepath"
	"sync"

	"resource-workbench/internal/store"
	"resource-workbench/internal/workbench"
)

//go:embed samples/verdant-reach.json
var embeddedSample []byte

type APIResponse struct {
	OK         bool                     `json:"ok"`
	View       *workbench.View          `json:"view,omitempty"`
	Comparison *workbench.CompareReport `json:"comparison,omitempty"`
	Report     string                   `json:"report,omitempty"`
	Error      *workbench.Error         `json:"error,omitempty"`
	Message    string                   `json:"message,omitempty"`
}

type App struct {
	mu       sync.Mutex
	ctx      context.Context
	engine   *workbench.Engine
	run      *workbench.RunResult
	store    *store.SQLiteStore
	regionAt string
	dbAt     string
}

func NewApp() *App {
	return &App{}
}

func (a *App) startup(ctx context.Context) {
	a.ctx = ctx
}

func (a *App) Initialize(regionPath, dbPath string) APIResponse {
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.ctx == nil {
		a.ctx = context.Background()
	}
	var region *workbench.Region
	var err *workbench.Error
	if regionPath == "" {
		region, err = workbench.LoadRegion(embeddedSample)
		regionPath = "(embedded) samples/verdant-reach.json"
	} else {
		data, readErr := os.ReadFile(regionPath)
		if readErr != nil {
			return failure(&workbench.Error{Code: workbench.CodeIO, Message: readErr.Error()})
		}
		region, err = workbench.LoadRegion(data)
	}
	if err != nil {
		return failure(err)
	}
	if dbPath == "" {
		dbPath = defaultDBPath()
	}
	var persistence workbench.PersistentStore
	var sqliteStore *store.SQLiteStore
	if dbPath != "none" && dbPath != ":memory:" {
		opened, openErr := store.Open(dbPath)
		if openErr != nil {
			return failure(&workbench.Error{Code: workbench.CodeStore, Message: openErr.Error()})
		}
		sqliteStore = opened
		persistence = opened
	}
	engine, engineErr := workbench.NewEngine(region, workbench.RuntimeConfig{}, persistence)
	if engineErr != nil {
		if sqliteStore != nil {
			_ = sqliteStore.Close()
		}
		return failure(engineErr)
	}
	run, runErr := engine.Run(a.ctx)
	if run == nil {
		if sqliteStore != nil {
			_ = sqliteStore.Close()
		}
		return failure(runErr)
	}
	if a.store != nil {
		_ = a.store.Close()
	}
	a.store = sqliteStore
	a.engine = engine
	a.run = run
	a.regionAt = regionPath
	a.dbAt = dbPath
	view := engine.View(run)
	response := APIResponse{OK: runErr == nil, View: &view, Message: "workspace loaded"}
	if runErr != nil {
		response.Error = runErr
	}
	return response
}

func (a *App) Refresh() APIResponse {
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.engine == nil {
		return failure(&workbench.Error{Code: workbench.CodeIO, Message: "workspace is not initialized"})
	}
	run, err := a.engine.Run(a.ctx)
	if run == nil {
		return failure(err)
	}
	a.run = run
	view := a.engine.View(run)
	response := APIResponse{OK: err == nil, View: &view}
	if err != nil {
		response.Error = err
	}
	return response
}

func (a *App) EditZone(req workbench.ZoneEditRequest) APIResponse {
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.engine == nil {
		return failure(&workbench.Error{Code: workbench.CodeIO, Message: "workspace is not initialized"})
	}
	run, err := a.engine.EditZone(a.ctx, req)
	if run == nil {
		return failure(err)
	}
	a.run = run
	view := a.engine.View(run)
	response := APIResponse{OK: err == nil, View: &view}
	if err != nil {
		response.Error = err
	}
	return response
}

func (a *App) EditNoCollect(req workbench.NoCollectEditRequest) APIResponse {
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.engine == nil {
		return failure(&workbench.Error{Code: workbench.CodeIO, Message: "workspace is not initialized"})
	}
	run, err := a.engine.EditNoCollect(a.ctx, req)
	if run == nil {
		return failure(err)
	}
	a.run = run
	view := a.engine.View(run)
	response := APIResponse{OK: err == nil, View: &view}
	if err != nil {
		response.Error = err
	}
	return response
}

func (a *App) RetryZone(zoneID string) APIResponse {
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.engine == nil {
		return failure(&workbench.Error{Code: workbench.CodeIO, Message: "workspace is not initialized"})
	}
	run, err := a.engine.RetryZone(a.ctx, zoneID)
	if run == nil {
		return failure(err)
	}
	a.run = run
	view := a.engine.View(run)
	response := APIResponse{OK: err == nil, View: &view}
	if err != nil {
		response.Error = err
	}
	return response
}

func (a *App) CompareFull() APIResponse {
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.engine == nil {
		return failure(&workbench.Error{Code: workbench.CodeIO, Message: "workspace is not initialized"})
	}
	report, err := a.engine.CompareFull(a.ctx)
	if report == nil {
		return failure(err)
	}
	response := APIResponse{OK: err == nil, Comparison: report, Message: "comparison complete"}
	if err != nil {
		response.Error = err
	}
	return response
}

func (a *App) ExportReport() APIResponse {
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.run == nil {
		return failure(&workbench.Error{Code: workbench.CodeIO, Message: "workspace is not initialized"})
	}
	payload := map[string]any{
		"run":  a.run,
		"view": a.engine.View(a.run),
	}
	data, marshalErr := json.MarshalIndent(payload, "", "  ")
	if marshalErr != nil {
		return failure(&workbench.Error{Code: workbench.CodeIO, Message: marshalErr.Error()})
	}
	return APIResponse{OK: true, Report: string(data), Message: "report exported"}
}

func (a *App) shutdown(_ context.Context) {
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.store != nil {
		_ = a.store.Close()
		a.store = nil
	}
}

func failure(err *workbench.Error) APIResponse {
	if err == nil {
		err = &workbench.Error{Code: workbench.CodeIO, Message: "unknown error"}
	}
	return APIResponse{OK: false, Error: err}
}

func defaultDBPath() string {
	base, err := os.UserConfigDir()
	if err != nil || base == "" {
		return "workbench.sqlite"
	}
	return filepath.Join(base, "resource-workbench", "checkpoints.sqlite")
}
