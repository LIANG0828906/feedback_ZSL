//go:build desktop || bindings

package main

import (
	"context"
	"embed"
	"fmt"

	"github.com/wailsapp/wails/v2"
	"github.com/wailsapp/wails/v2/pkg/options"
	"github.com/wailsapp/wails/v2/pkg/options/assetserver"

	"volumediag/internal/engine"
	"volumediag/internal/volume"
	"volumediag/internal/workbench"
)

//go:embed all:frontend/src
var assets embed.FS

type App struct {
	ctx context.Context
	wb  *workbench.Workbench
}

func NewApp() *App {
	return &App{wb: workbench.New()}
}

func (a *App) startup(ctx context.Context) {
	a.ctx = ctx
	a.wb.SetContext(ctx)
}

func (a *App) shutdown(ctx context.Context) {
	_ = a.wb.Close()
}

func (a *App) Bootstrap(sceneDir, dbPath string, seed int64) workbench.ActionResult {
	return a.wb.Bootstrap(sceneDir, dbPath, seed)
}

func (a *App) GenerateSample(outDir string, seed int64) workbench.ActionResult {
	return a.wb.GenerateSample(outDir, seed)
}

func (a *App) RenderFull() workbench.ActionResult {
	return a.wb.RenderFull()
}

func (a *App) RenderPreview() workbench.ActionResult {
	return a.wb.RenderPreview()
}

func (a *App) RenderResume() workbench.ActionResult {
	return a.wb.RenderResume()
}

func (a *App) ApplyEdit(req engine.EditRequest) workbench.ActionResult {
	return a.wb.ApplyEdit(req)
}

func (a *App) Configure(cfg workbench.Config) workbench.ActionResult {
	return a.wb.Configure(cfg)
}

func (a *App) Pause() workbench.ActionResult {
	return a.wb.Pause()
}

func (a *App) ResumeRendering() workbench.ActionResult {
	return a.wb.ResumeRendering()
}

func (a *App) Compare() workbench.ActionResult {
	return a.wb.Compare()
}

func (a *App) VerifyTruth() workbench.ActionResult {
	return a.wb.VerifyTruth()
}

func (a *App) RetryFailed() workbench.ActionResult {
	return a.wb.RetryFailed()
}

func (a *App) State() workbench.ActionResult {
	return a.wb.State()
}

func (a *App) Block(x, y int) workbench.BlockDetail {
	return a.wb.Block(x, y)
}

func (a *App) LightCoverage(lightID string) []volume.BlockKey {
	return a.wb.LightCoverage(lightID)
}

func (a *App) ScenePath() string { return a.wb.ScenePath() }
func (a *App) DBPath() string    { return a.wb.DBPath() }

func main() {
	app := NewApp()
	err := wails.Run(&options.App{
		Title:            "Screen-Space Volumetric Light Diagnostics",
		Width:            1500,
		Height:           960,
		MinWidth:         1180,
		MinHeight:        760,
		DisableResize:    false,
		BackgroundColour: &options.RGBA{R: 9, G: 13, B: 20, A: 1},
		AssetServer: &assetserver.Options{
			Assets: assets,
		},
		OnStartup:  app.startup,
		OnShutdown: app.shutdown,
		Bind: []interface{}{
			app,
		},
	})
	if err != nil {
		fmt.Println("Error:", err.Error())
	}
}
