package main

import (
	"embed"
	"log"

	"github.com/wailsapp/wails/v2"
	"github.com/wailsapp/wails/v2/pkg/options"
	"github.com/wailsapp/wails/v2/pkg/options/assetserver"

	"hdrproof/desktop/internal/workbench"
)

//go:embed all:frontend/dist
var assets embed.FS

func main() {
	app := workbench.New()
	err := wails.Run(&options.App{
		Title:       "HDR Proof Workbench",
		Width:       1500,
		Height:      940,
		MinWidth:    1150,
		MinHeight:   760,
		AssetServer: &assetserver.Options{Assets: assets},
		BackgroundColour: &options.RGBA{R: 14, G: 18, B: 24, A: 1},
		OnStartup:        app.Startup,
		Bind:             []interface{}{app},
	})
	if err != nil {
		log.Fatal(err)
	}
}
