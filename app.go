package main

import (
	"context"

	"strokeforge/internal/engine"
	"strokeforge/internal/generator"
	"strokeforge/internal/model"
	"strokeforge/internal/service"
)

// App is the Wails-exposed application facade.
type App struct {
	ctx context.Context
	svc *service.Service
}

// NewApp constructs the desktop binding.
func NewApp() *App {
	svc, err := service.New(generator.DefaultSeed, engine.DefaultPoolConfig(),
		"artifacts/checkpoint-desktop")
	if err != nil {
		panic(err)
	}
	if _, err := svc.FullRender(); err != nil {
		panic(err)
	}
	return &App{svc: svc}
}

// startup is called when the app launches.
func (a *App) startup(ctx context.Context) { a.ctx = ctx }

// Initial returns the full diagnostics snapshot for the frontend.
func (a *App) Initial() (*service.SnapshotView, error) { return a.svc.Snapshot() }

// UpdateParams applies a parameter edit and returns invalidation + compare.
func (a *App) UpdateParams(params model.StrokeParams) (map[string]any, error) {
	entries, stats, diff, err := a.svc.Incremental(params)
	if err != nil {
		return map[string]any{"error": err.Error()}, err
	}
	return map[string]any{
		"invalidation": entries,
		"stats":        stats,
		"compare":      diff,
	}, nil
}

// Diagnose triggers one injected-fault scenario.
func (a *App) Diagnose(kind string, glyphID string, segmentID int) error {
	return a.svc.Diagnose(kind, glyphID, segmentID)
}

// FullRender forces a from-scratch render.
func (a *App) FullRender() (*engine.RenderStats, error) { return a.svc.FullRender() }

// AlgorithmVersion reports the build's algorithm tag.
func (a *App) AlgorithmVersion() string { return model.AlgorithmVersion }
