package main

import (
	"context"
	"fmt"
	"os"
	"path/filepath"

	"github.com/wailsapp/wails/v2"
	"github.com/wailsapp/wails/v2/pkg/options"
	"github.com/wailsapp/wails/v2/pkg/options/assetserver"

	"terrainbake/diagnostic"
)

func main() {
	dbPath := filepath.Join("artifacts", "wails-diagnostic.sqlite")
	controller, err := diagnostic.New(424242, dbPath)
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	defer controller.Close()
	err = wails.Run(&options.App{
		Title:       "Terrain Bake Diagnostics",
		Width:       1280,
		Height:      860,
		MinWidth:    980,
		MinHeight:   720,
		AssetServer: &assetserver.Options{Handler: diagnostic.NewHTTPHandler(controller)},
		OnShutdown: func(_ context.Context) {
			_ = controller.Close()
		},
	})
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}
