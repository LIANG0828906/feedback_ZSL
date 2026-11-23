package main

import (
	"context"
	"embed"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"

	"github.com/wailsapp/wails/v2"
	"github.com/wailsapp/wails/v2/pkg/options"
	"github.com/wailsapp/wails/v2/pkg/options/assetserver"
	"github.com/wailsapp/wails/v2/pkg/options/windows"

	"tacticalworkbench/internal/sample"
	"tacticalworkbench/pkg/tactical"
	"tacticalworkbench/pkg/workbench"
)

//go:embed all:frontend/dist
var assets embed.FS

type App struct {
	ctx      context.Context
	service  *workbench.Service
	initErr  error
	dbPath   string
	scenario string
}

func NewApp() *App {
	scenario := os.Getenv("TACTICAL_SCENARIO")
	dbPath := os.Getenv("TACTICAL_DB")
	if dbPath == "" {
		configDir, err := os.UserConfigDir()
		if err != nil {
			configDir = "."
		}
		dbPath = filepath.Join(configDir, "tactical-workbench", "workbench.db")
	}
	app := &App{scenario: scenario, dbPath: dbPath}
	if scenario != "" {
		app.service = workbench.NewService(scenario, dbPath)
		return app
	}
	generated, err := sample.Generate(sample.DefaultSeed)
	if err != nil {
		app.initErr = err
		app.service = workbench.NewServiceWithScenario(&tactical.Scenario{}, dbPath)
		return app
	}
	app.service = workbench.NewServiceWithScenario(generated, dbPath)
	return app
}

func (a *App) startup(ctx context.Context) {
	a.ctx = ctx
	if a.initErr != nil {
		return
	}
	if err := os.MkdirAll(filepath.Dir(a.dbPath), 0o755); err != nil {
		a.initErr = err
		return
	}
	_, a.initErr = a.service.Initialize(ctx)
}

func (a *App) shutdown(ctx context.Context) {
	a.service.Shutdown()
}

func (a *App) GetState() (workbench.ViewState, error) {
	if a.initErr != nil {
		return workbench.ViewState{}, a.initErr
	}
	return a.service.State(), nil
}

func (a *App) SetCellCost(x, y int, cost int64) (workbench.EditResultView, error) {
	if a.initErr != nil {
		return workbench.EditResultView{}, a.initErr
	}
	return a.service.SetCellCost(a.context(), x, y, cost)
}

func (a *App) SetRegionActive(regionID string, active bool) (workbench.EditResultView, error) {
	if a.initErr != nil {
		return workbench.EditResultView{}, a.initErr
	}
	return a.service.SetRegionActive(a.context(), regionID, active)
}

func (a *App) RetryMember(memberID string) (workbench.ViewState, error) {
	if a.initErr != nil {
		return workbench.ViewState{}, a.initErr
	}
	return a.service.RetryMember(a.context(), memberID)
}

func (a *App) Pause() (workbench.ViewState, error) {
	return a.service.Pause(a.context())
}

func (a *App) Resume() (workbench.ViewState, error) {
	return a.service.Resume(a.context())
}

func (a *App) Restart() (workbench.ViewState, error) {
	return a.service.Restart(a.context())
}

func (a *App) GetPaths() map[string]string {
	return map[string]string{
		"scenario": a.scenario,
		"sqlite":   a.dbPath,
	}
}

func (a *App) context() context.Context {
	if a.ctx != nil {
		return a.ctx
	}
	return context.Background()
}

func main() {
	app := NewApp()
	dist, err := fs.Sub(assets, "frontend/dist")
	if err != nil {
		fmt.Fprintln(os.Stderr, "embedded frontend:", err)
		os.Exit(1)
	}
	err = wails.Run(&options.App{
		Title:             "Tactical Formation Workbench",
		Width:             1440,
		Height:            920,
		MinWidth:          1180,
		MinHeight:         720,
		DisableResize:     false,
		Fullscreen:        false,
		Frameless:         false,
		StartHidden:       false,
		HideWindowOnClose: false,
		BackgroundColour:  &options.RGBA{R: 10, G: 15, B: 20, A: 255},
		AssetServer:       &assetserver.Options{Assets: dist},
		OnStartup:         app.startup,
		OnShutdown:        app.shutdown,
		Bind:              []any{app},
		Windows: &windows.Options{
			WebviewIsTransparent: false,
			WindowIsTranslucent:  false,
			DisableWindowIcon:    false,
		},
	})
	if err != nil {
		fmt.Fprintln(os.Stderr, "tactical workbench:", err)
		os.Exit(1)
	}
}
