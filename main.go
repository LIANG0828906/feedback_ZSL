//go:build !headless

package main

import (
	"embed"
	"log"

	"github.com/wailsapp/wails/v2"
	"github.com/wailsapp/wails/v2/pkg/options"
	"github.com/wailsapp/wails/v2/pkg/options/assetserver"
)

//go:embed all:frontend/dist
var assets embed.FS

func main() {
	app := NewApp()
	err := wails.Run(&options.App{
		Title:            "StageCue Workbench",
		Width:            1440,
		Height:           900,
		MinWidth:         1100,
		MinHeight:        720,
		DisableResize:    false,
		BackgroundColour: &options.RGBA{R: 12, G: 16, B: 24, A: 255},
		AssetServer:      &assetserver.Options{Assets: assets},
		OnStartup:        app.startup,
		Bind:             []any{app},
	})
	if err != nil {
		log.Fatal(err)
	}
}
