//go:build wails

package main

import (
	"context"
	"os"
	"path/filepath"

	"github.com/wailsapp/wails/v2/pkg/runtime"

	"occlusionlab/internal/model"
	"occlusionlab/internal/verify"
	"occlusionlab/internal/workbench"
)

type App struct {
	ctx     context.Context
	service *workbench.Service
}

func NewApp(dataDir string) *App {
	return &App{service: workbench.NewService(dataDir)}
}

func (a *App) startup(ctx context.Context) {
	a.ctx = ctx
}

func (a *App) shutdown(context.Context) {
	a.service.Close()
}

func (a *App) Bootstrap() (*workbench.Dashboard, *model.AppError) {
	return a.service.Bootstrap()
}

func (a *App) Dashboard() (*workbench.Dashboard, *model.AppError) {
	return a.service.Dashboard()
}

func (a *App) Generate(opts workbench.GenerateOptions) (*workbench.Dashboard, *model.AppError) {
	return a.service.Regenerate(opts)
}

func (a *App) UpdateParams(opts workbench.RuntimeOptions) (*workbench.Dashboard, *model.AppError) {
	return a.service.UpdateParams(opts)
}

func (a *App) Step() (*workbench.Dashboard, *model.AppError) {
	return a.service.Step()
}

func (a *App) ProcessAll() (*workbench.Dashboard, *model.AppError) {
	return a.service.ProcessAll()
}

func (a *App) SetPaused(paused bool) (*workbench.Dashboard, *model.AppError) {
	return a.service.SetPaused(paused)
}

func (a *App) Retry(window int) (*workbench.Dashboard, *model.AppError) {
	return a.service.Retry(window)
}

func (a *App) SelectWindow(window int) (*workbench.Dashboard, *model.AppError) {
	return a.service.SelectWindow(window)
}

func (a *App) MoveCamera(window int, dx, dy, dz float64) (*workbench.Dashboard, *model.AppError) {
	return a.service.MoveCamera(window, dx, dy, dz)
}

func (a *App) Validate() (*verify.Report, *model.AppError) {
	return a.service.Validate()
}

func (a *App) Reset() (*workbench.Dashboard, *model.AppError) {
	return a.service.Reset()
}

func (a *App) ExportProject() (string, *model.AppError) {
	return a.service.ExportProject()
}

func (a *App) DataDir() string {
	return a.service.DataDir()
}

func (a *App) Quit() {
	runtime.Quit(a.ctx)
}

func defaultDataDir() string {
	if value := os.Getenv("OCCLUSIONLAB_DATA"); value != "" {
		return value
	}
	if config, err := os.UserConfigDir(); err == nil && config != "" {
		return filepath.Join(config, "OcclusionLab")
	}
	return filepath.Join(".", "data")
}
