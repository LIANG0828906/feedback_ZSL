package main

import (
	"log"
	"os"
	"path/filepath"

	"github.com/wailsapp/wails/v2"
	"github.com/wailsapp/wails/v2/pkg/options"
	"github.com/wailsapp/wails/v2/pkg/options/assetserver"

	"volumelab/pkg/gui"
	"volumelab/pkg/webui"
)

func main() {
	app := gui.NewApp(findDefaultManifest())
	err := wails.Run(&options.App{
		Title:            "VolumeLab · 确定性医学影像工作站",
		Width:            1600,
		Height:           980,
		MinWidth:         1120,
		MinHeight:        720,
		DisableResize:    false,
		Frameless:        false,
		BackgroundColour: &options.RGBA{R: 7, G: 16, B: 21, A: 1},
		AssetServer: &assetserver.Options{
			Assets: webui.FS,
		},
		OnStartup:  app.Startup,
		OnShutdown: app.Shutdown,
		Bind: []interface{}{
			app,
		},
	})
	if err != nil {
		log.Fatal(err)
	}
}

func findDefaultManifest() string {
	if value := os.Getenv("VOLUMELAB_MANIFEST"); value != "" {
		return value
	}
	candidates := []string{
		filepath.Join("samples", "manifest.json"),
		filepath.Join("..", "samples", "manifest.json"),
		filepath.Join("..", "..", "samples", "manifest.json"),
	}
	if executable, err := os.Executable(); err == nil {
		exeDir := filepath.Dir(executable)
		candidates = append(candidates,
			filepath.Join(exeDir, "samples", "manifest.json"),
			filepath.Join(exeDir, "..", "samples", "manifest.json"),
			filepath.Join(exeDir, "..", "..", "samples", "manifest.json"),
			filepath.Join(exeDir, "..", "..", "..", "samples", "manifest.json"),
		)
	}
	for _, candidate := range candidates {
		if _, err := os.Stat(candidate); err == nil {
			abs, err := filepath.Abs(candidate)
			if err == nil {
				return abs
			}
			return candidate
		}
	}
	return ""
}
