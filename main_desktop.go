//go:build desktop

package main

import (
	"embed"
	"log"
	"os"
	"path/filepath"

	"github.com/wailsapp/wails/v2"
	"github.com/wailsapp/wails/v2/pkg/options"
	"github.com/wailsapp/wails/v2/pkg/options/assetserver"

	"texlab/internal/workbench"
)

//go:embed all:frontend/dist
var assets embed.FS

func main() {
	sampleRoot := os.Getenv("TEXLAB_SAMPLES")
	if sampleRoot == "" {
		sampleRoot = filepath.Join(userDataDir(), "samples")
	}
	dataDir := os.Getenv("TEXLAB_DATA")
	if dataDir == "" {
		dataDir = filepath.Join(userDataDir(), "data")
	}
	svc, err := workbench.NewService(sampleRoot, dataDir, 4, 262144)
	if err != nil {
		log.Fatal(err)
	}
	app := NewApp(svc)
	err = wails.Run(&options.App{
		Title:     "TexLab · Bit Depth & Channel Workbench",
		Width:     1480,
		Height:    940,
		MinWidth:  1120,
		MinHeight: 720,
		AssetServer: &assetserver.Options{
			Assets: assets,
		},
		BackgroundColour: &options.RGBA{R: 8, G: 13, B: 19, A: 1},
		Bind:             []interface{}{app},
	})
	if err != nil {
		log.Fatal(err)
	}
}

func userDataDir() string {
	if v := os.Getenv("TEXLAB_DATA_HOME"); v != "" {
		return v
	}
	base, err := os.UserConfigDir()
	if err != nil {
		base = "."
	}
	return filepath.Join(base, "TexLab")
}
