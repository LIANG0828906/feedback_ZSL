package main

import (
	"context"
	"embed"
	"log"
	"os"
	"path/filepath"

	"edgeworkbench/internal/workbench"

	"github.com/wailsapp/wails/v2"
	"github.com/wailsapp/wails/v2/pkg/options"
	"github.com/wailsapp/wails/v2/pkg/options/assetserver"
)

//go:embed all:frontend/dist
var assets embed.FS

type App struct {
	*workbench.Workbench
}

func main() {
	app := &App{Workbench: workbench.New(defaultDataDir())}
	err := wails.Run(&options.App{
		Title:            "MorphEdge Studio",
		Width:            1500,
		Height:           940,
		MinWidth:         1120,
		MinHeight:        720,
		DisableResize:    false,
		Frameless:        false,
		BackgroundColour: &options.RGBA{R: 8, G: 12, B: 18, A: 1},
		AssetServer:      &assetserver.Options{Assets: assets},
		OnStartup: func(ctx context.Context) {
			if _, err := app.Bootstrap(); err != nil {
				log.Printf("bootstrap failed: %v", err)
			}
		},
		Bind: []interface{}{app},
	})
	if err != nil {
		log.Fatal(err)
	}
}

func defaultDataDir() string {
	if dir := os.Getenv("EDGE_WORKBENCH_DATA"); dir != "" {
		return dir
	}
	base, err := os.UserConfigDir()
	if err != nil || base == "" {
		base = "."
	}
	return filepath.Join(base, "MorphEdgeStudio")
}
