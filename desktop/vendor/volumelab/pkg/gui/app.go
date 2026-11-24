package gui

import (
	"context"

	"volumelab/pkg/service"
)

type App struct {
	ctx context.Context
	svc *service.Service
}

func NewApp(manifestPath string) *App {
	return &App{
		svc: service.New(service.Options{
			ManifestPath:    manifestPath,
			MaxParallelism:  3,
			MaxBufferPixels: 16384,
		}),
	}
}

func (a *App) Startup(ctx context.Context) {
	a.ctx = ctx
}

func (a *App) Shutdown(context.Context) {
	_ = a.svc.Invoke(context.Background(), "close", "{}")
}

func (a *App) Invoke(action, payload string) service.APIResult {
	ctx := a.ctx
	if ctx == nil {
		ctx = context.Background()
	}
	if action == "cancel" {
		ctx = context.Background()
	}
	return a.svc.Invoke(ctx, action, payload)
}

func (a *App) Cancel() service.APIResult {
	return a.svc.Invoke(context.Background(), "cancel", "{}")
}
