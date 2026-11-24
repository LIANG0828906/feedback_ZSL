package main

import (
	"embed"

	"github.com/wailsapp/wails/v2"
	"github.com/wailsapp/wails/v2/pkg/options"
	"github.com/wailsapp/wails/v2/pkg/options/assetserver"
)

//go:embed all:frontend/dist
var assets embed.FS

func main() {
	app := NewApp("data")
	err := wails.Run(&options.App{
		Title:       "SIR 空间声学脉冲响应工作台",
		Width:       1280,
		Height:      860,
		AssetServer: &assetserver.Options{Assets: assets},
		OnStartup:   app.startup,
		Bind:        []any{app},
	})
	if err != nil {
		panic(err)
	}
}
