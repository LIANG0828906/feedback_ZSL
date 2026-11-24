package main

import (
	"context"
	"encoding/base64"
	"fmt"
	"log"

	"github.com/wailsapp/wails/v2"
	"github.com/wailsapp/wails/v2/pkg/options"
	"github.com/wailsapp/wails/v2/pkg/options/assetserver"
	"github.com/wailsapp/wails/v2/pkg/options/linux"

	vstage "virtualstage/app"
	"virtualstage/pipeline"
	"virtualstage/webui"
)

type App struct {
	workDir string
	workers int
	service *vstage.Service
	ctx     context.Context
}

type EditResult struct {
	Invalidation pipeline.InvalidationList `json:"invalidation"`
	Snapshot     vstage.Snapshot           `json:"snapshot"`
}

func NewApp(workDir string, workers int) *App {
	return &App{workDir: workDir, workers: workers}
}

func (a *App) startup(ctx context.Context) {
	a.ctx = ctx
	if _, err := a.open(); err != nil {
		log.Printf("open pipeline: %v", err)
	}
}

func (a *App) shutdown(ctx context.Context) {
	if a.service != nil {
		_ = a.service.Close()
		a.service = nil
	}
}

func (a *App) open() (*vstage.Service, error) {
	if a.service != nil {
		return a.service, nil
	}
	service, err := vstage.OpenService(vstage.Config{
		WorkDir: a.workDir, Workers: a.workers, MaxFrameBytes: 64 << 20,
	})
	if err != nil {
		return nil, err
	}
	a.service = service
	return service, nil
}

func (a *App) GetState() (vstage.Snapshot, error) {
	service, err := a.open()
	if err != nil {
		return vstage.Snapshot{}, err
	}
	return service.Snapshot(), nil
}

func (a *App) RunFull() (vstage.Snapshot, error) {
	service, err := a.open()
	if err != nil {
		return vstage.Snapshot{}, err
	}
	return service.RunFull(a.context())
}

func (a *App) ApplyEdit(edit pipeline.EditRequest) (EditResult, error) {
	service, err := a.open()
	if err != nil {
		return EditResult{}, err
	}
	invalidation, err := service.ApplyIncremental(a.context(), edit)
	if err != nil {
		return EditResult{}, err
	}
	return EditResult{Invalidation: invalidation, Snapshot: service.Snapshot()}, nil
}

func (a *App) ExportVersion(version int) (vstage.ExportVerification, error) {
	service, err := a.open()
	if err != nil {
		return vstage.ExportVerification{}, err
	}
	return service.ExportAndVerify(a.context(), version)
}

func (a *App) ValidateTruth() (vstage.TruthValidation, error) {
	service, err := a.open()
	if err != nil {
		return vstage.TruthValidation{}, err
	}
	return service.ValidateTruth(a.context())
}

func (a *App) GetHeatmap(window, frame, cellsX, cellsY int) (vstage.Heatmap, error) {
	service, err := a.open()
	if err != nil {
		return vstage.Heatmap{}, err
	}
	return service.Heatmap(window, frame, cellsX, cellsY)
}

func (a *App) FrameDataURL(window, frame int) (string, error) {
	service, err := a.open()
	if err != nil {
		return "", err
	}
	data, err := service.FramePNG(window, frame)
	if err != nil {
		return "", err
	}
	return "data:image/png;base64," + base64.StdEncoding.EncodeToString(data), nil
}

func (a *App) context() context.Context {
	if a.ctx != nil {
		return a.ctx
	}
	return context.Background()
}

func main() {
	app := NewApp("work", 4)
	err := wails.Run(&options.App{
		Title:     "Virtual Stage Composite",
		Width:     1440,
		Height:    920,
		MinWidth:  980,
		MinHeight: 680,
		AssetServer: &assetserver.Options{
			Assets: webui.FS(),
		},
		BackgroundColour: options.NewRGB(11, 13, 16),
		OnStartup:        app.startup,
		OnShutdown:       app.shutdown,
		Bind:             []any{app},
		Linux: &linux.Options{
			ProgramName:      "virtual-stage",
			WebviewGpuPolicy: linux.WebviewGpuPolicyOnDemand,
		},
	})
	if err != nil {
		fmt.Println("virtual stage desktop:", err)
	}
}
