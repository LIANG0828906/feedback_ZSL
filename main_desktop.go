//go:build !web

package main

import (
	"context"
	"embed"
	"encoding/json"
	"io/fs"
	"log"
	"path/filepath"

	"github.com/wailsapp/wails/v2"
	"github.com/wailsapp/wails/v2/pkg/options"
	"github.com/wailsapp/wails/v2/pkg/options/assetserver"
	"github.com/wailsapp/wails/v2/pkg/options/windows"

	"strata/internal/model"
	"strata/internal/service"
)

//go:embed all:frontend/dist
var desktopAssets embed.FS

type DesktopApp struct {
	svc *service.Service
}

func (a *DesktopApp) Dispatch(action, payload string) string {
	if payload == "" {
		payload = "{}"
	}
	env := a.svc.Dispatch(action, json.RawMessage(payload))
	data, err := json.Marshal(env)
	if err != nil {
		fallback, _ := json.Marshal(service.Envelope{OK: false, Error: model.AsAppError(err)})
		return string(fallback)
	}
	return string(data)
}

func (a *DesktopApp) Shutdown() {
	_ = a.svc.Close()
}

func main() {
	root, err := filepath.Abs(".")
	if err != nil {
		log.Fatal(err)
	}
	svc, err := service.New(root)
	if err != nil {
		log.Fatal(err)
	}
	app := &DesktopApp{svc: svc}
	assets, err := fs.Sub(desktopAssets, "frontend/dist")
	if err != nil {
		log.Fatal(err)
	}
	err = wails.Run(&options.App{
		Title:     "STRATA · Point Cloud Reconstruction Studio",
		Width:     1440,
		Height:    900,
		MinWidth:  1180,
		MinHeight: 720,
		AssetServer: &assetserver.Options{
			Assets: assets,
		},
		Bind: []interface{}{app},
		OnShutdown: func(ctx context.Context) {
			_ = svc.Close()
		},
		Windows: &windows.Options{
			WebviewIsTransparent: false,
			WindowIsTranslucent:  false,
		},
	})
	if err != nil {
		log.Fatal(err)
	}
}
