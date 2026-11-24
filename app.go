package main

import (
	"texlab/internal/workbench"
)

// App is the Wails binding surface. All heavy lifting stays in the testable
// workbench/engine packages.
type App struct {
	svc *workbench.Service
}

func NewApp(svc *workbench.Service) *App {
	return &App{svc: svc}
}

func (a *App) ListSamples() ([]workbench.SampleInfo, error) {
	return a.svc.ListSamples()
}

func (a *App) LoadSample(name string) (*workbench.State, error) {
	return a.svc.LoadSample(name)
}

func (a *App) ApplyEdit(req workbench.EditRequest) (*workbench.State, error) {
	return a.svc.ApplyEdit(req)
}

func (a *App) Reorder(req workbench.ReorderRequest) (*workbench.State, error) {
	return a.svc.Reorder(req)
}

func (a *App) RetrySegment(req workbench.RetryRequest) (*workbench.State, error) {
	return a.svc.RetrySegment(req)
}

func (a *App) ComparePaths() (*workbench.CompareResult, error) {
	return a.svc.ComparePaths()
}

func (a *App) Export() (*workbench.ExportResult, error) {
	return a.svc.Export()
}
