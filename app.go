//go:build windows

package main

import (
	"context"
	"embed"
	"os"
	"path/filepath"

	"atelier/internal/apperr"
	"atelier/internal/workspace"
)

//go:embed all:frontend/dist
var assets embed.FS

type App struct {
	ctx     context.Context
	service *workspace.Service
	err     error
}

func NewApp() *App {
	return &App{}
}

func (a *App) startup(ctx context.Context) {
	a.ctx = ctx
	cwd, err := os.Getwd()
	if err != nil {
		a.err = err
		return
	}
	a.service, a.err = workspace.New(filepath.Join(cwd, "atelier-checkpoints.db"))
}

func (a *App) shutdown(ctx context.Context) {
	if a.service != nil {
		_ = a.service.Close()
	}
}

func (a *App) ready() *workspace.APIResponse {
	if a.err != nil || a.service == nil {
		msg := "workspace initialization failed"
		if a.err != nil {
			msg = a.err.Error()
		}
		return &workspace.APIResponse{
			OK: false, Code: apperr.CodeIO, Message: msg,
			Error: &apperr.Error{Code: apperr.CodeIO, Message: msg},
		}
	}
	return nil
}

func (a *App) Bootstrap() workspace.APIResponse {
	if r := a.ready(); r != nil {
		return *r
	}
	return a.service.Bootstrap(a.ctx)
}

func (a *App) Catalog() workspace.APIResponse {
	return workspace.APIResponse{OK: true, Data: workspace.Catalog()}
}

func (a *App) LoadRegion(seed int64) workspace.APIResponse {
	if r := a.ready(); r != nil {
		return *r
	}
	return a.service.LoadRegion(a.ctx, seed)
}

func (a *App) LoadScenarioJSON(payload string) workspace.APIResponse {
	if r := a.ready(); r != nil {
		return *r
	}
	return a.service.LoadScenarioJSON(a.ctx, payload)
}

func (a *App) Snapshot() workspace.APIResponse {
	if r := a.ready(); r != nil {
		return *r
	}
	return a.service.Snapshot()
}

func (a *App) UpdateZone(req workspace.UpdateZoneRequest) workspace.APIResponse {
	if r := a.ready(); r != nil {
		return *r
	}
	return a.service.UpdateZone(a.ctx, req)
}

func (a *App) MoveExclusion(req workspace.MoveExclusionRequest) workspace.APIResponse {
	if r := a.ready(); r != nil {
		return *r
	}
	return a.service.MoveExclusion(a.ctx, req)
}

func (a *App) UpdateZoneBoundary(req workspace.BoundaryRequest) workspace.APIResponse {
	if r := a.ready(); r != nil {
		return *r
	}
	return a.service.UpdateZoneBoundary(a.ctx, req)
}

func (a *App) RetryZone(zoneID string) workspace.APIResponse {
	if r := a.ready(); r != nil {
		return *r
	}
	return a.service.RetryZone(a.ctx, zoneID)
}

func (a *App) FullRebuild() workspace.APIResponse {
	if r := a.ready(); r != nil {
		return *r
	}
	return a.service.FullRebuild(a.ctx)
}

func (a *App) Compare() workspace.APIResponse {
	if r := a.ready(); r != nil {
		return *r
	}
	return a.service.Compare(a.ctx)
}

func (a *App) Cancel() workspace.APIResponse {
	if r := a.ready(); r != nil {
		return *r
	}
	return a.service.Cancel()
}

func (a *App) Export() workspace.APIResponse {
	if r := a.ready(); r != nil {
		return *r
	}
	return a.service.Export(a.ctx)
}

func (a *App) SaveGeneratedSample(seed int64) workspace.APIResponse {
	if r := a.ready(); r != nil {
		return *r
	}
	return a.service.SaveGeneratedSample(a.ctx, seed)
}
