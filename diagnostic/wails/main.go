package main

import (
	"flag"
	"log"
	"os"
	"path/filepath"

	"github.com/wailsapp/wails/v2"
	"github.com/wailsapp/wails/v2/pkg/options"
	"github.com/wailsapp/wails/v2/pkg/options/assetserver"

	"qkernel/diag"
)

func main() {
	root := flag.String("root", defaultRoot(), "persisted qkernel output root")
	runID := flag.String("run", "", "pinned run id; defaults to verification.json or latest")
	flag.Parse()

	handler := (&diag.Server{Root: *root, RunID: *runID}).Handler()
	err := wails.Run(&options.App{
		Title:     "QKernel 画质劣化诊断台",
		Width:     1480,
		Height:    980,
		MinWidth:  1100,
		MinHeight: 720,
		AssetServer: &assetserver.Options{
			Handler: handler,
		},
		BackgroundColour: &options.RGBA{R: 11, G: 16, B: 32, A: 255},
	})
	if err != nil {
		log.Fatal(err)
	}
}

func defaultRoot() string {
	if env := os.Getenv("QKERNEL_ROOT"); env != "" {
		return env
	}
	if _, err := os.Stat("demo-output"); err == nil {
		return "demo-output"
	}
	return filepath.Join("..", "..", "demo-output")
}
