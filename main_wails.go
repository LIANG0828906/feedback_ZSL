//go:build wails

package main

import (
	"io/fs"
	"log"
	"os"

	"github.com/wailsapp/wails/v2"
	"github.com/wailsapp/wails/v2/pkg/options"
	"github.com/wailsapp/wails/v2/pkg/options/assetserver"

	"cadlab/frontend"
	"cadlab/internal/app"
	"cadlab/internal/engine"
	"cadlab/internal/sample"
)

func main() {
	cwd, _ := os.Getwd()
	opts := engine.DefaultOptions(cwd)
	a, err := app.New(opts, sample.DefaultSeed)
	if err != nil {
		log.Fatalf("workbench init: %v", err)
	}
	assets, err := fs.Sub(frontend.FS, "dist")
	if err != nil {
		log.Fatal(err)
	}
	err = wails.Run(&options.App{
		Title:            "CADLab Surface Diagnostics",
		Width:            1460,
		Height:           920,
		MinWidth:         1040,
		MinHeight:        720,
		BackgroundColour: &options.RGBA{R: 8, G: 12, B: 15, A: 255},
		AssetServer:      &assetserver.Options{Assets: assets},
		OnStartup:        a.Startup,
		OnShutdown:       a.Shutdown,
		Bind:             []any{a},
	})
	if err != nil {
		log.Fatal(err)
	}
}
