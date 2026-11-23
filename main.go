//go:build desktop

package main

import (
	"context"
	"encoding/json"
	"log"
	"os"
	"path/filepath"
	"strings"

	"github.com/wailsapp/wails/v2"
	"github.com/wailsapp/wails/v2/pkg/options"
	"github.com/wailsapp/wails/v2/pkg/options/assetserver"
	"github.com/wailsapp/wails/v2/pkg/runtime"

	"ocrworkbench/internal/domain"
	"ocrworkbench/internal/lab"
	"ocrworkbench/internal/webui"
)

type App struct {
	ctx     context.Context
	service *lab.Service
}

func NewDesktopApp() (*App, error) {
	base, err := os.UserConfigDir()
	if err != nil {
		base = "."
	}
	root := filepath.Join(base, "OCRWorkbench")
	service, appErr := lab.New(
		filepath.Join(root, "samples"),
		filepath.Join(root, "data", "workbench.db"),
		domain.DefaultBaseSeed,
		true,
	)
	if appErr != nil {
		return nil, appErr
	}
	app := &App{service: service}
	service.Emit = func(event string, data any) {
		if app.ctx != nil {
			runtime.EventsEmit(app.ctx, event, data)
		}
	}
	return app, nil
}

func (a *App) startup(ctx context.Context) {
	a.ctx = ctx
}

func (a *App) shutdown(ctx context.Context) {
	if a.service != nil {
		a.service.Close()
	}
}

// Call is the single Wails-bound API entry. It returns the same JSON envelope
// as the HTTP lab, so the frontend has one implementation for both runtimes.
func (a *App) Call(method string, payload string) (string, error) {
	raw := json.RawMessage(strings.TrimSpace(payload))
	if len(raw) == 0 {
		raw = json.RawMessage("{}")
	}
	response := a.service.Handle(method, raw)
	data, err := json.Marshal(response)
	if err != nil {
		return "", err
	}
	return string(data), nil
}

func main() {
	app, err := NewDesktopApp()
	if err != nil {
		log.Fatal(err)
	}
	err = wails.Run(&options.App{
		Title:             "OCR Workbench · Game Media Lab",
		Width:             1480,
		Height:            940,
		MinWidth:          1080,
		MinHeight:         720,
		DisableResize:     false,
		Frameless:         false,
		StartHidden:       false,
		HideWindowOnClose: false,
		BackgroundColour:  &options.RGBA{R: 8, G: 11, B: 16, A: 255},
		AssetServer: &assetserver.Options{
			Assets: webui.FS(),
		},
		OnStartup:  app.startup,
		OnShutdown: app.shutdown,
		Bind: []interface{}{
			app,
		},
	})
	if err != nil {
		log.Fatal(err)
	}
}
