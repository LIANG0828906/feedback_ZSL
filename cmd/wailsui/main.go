//go:build windows

package main

import (
	"log"

	"github.com/wailsapp/wails/v2"
	"github.com/wailsapp/wails/v2/pkg/options"
	"github.com/wailsapp/wails/v2/pkg/options/assetserver"

	"iconcut/ui"
)

func main() {
	server, err := ui.NewServer(".iconcut-workspace")
	if err != nil {
		log.Fatal(err)
	}
	err = wails.Run(&options.App{
		Title:     "IconCut 游戏 UI 图标切图验收台",
		Width:     1480,
		Height:    980,
		MinWidth:  1120,
		MinHeight: 760,
		AssetServer: &assetserver.Options{
			Handler: server.Handler(),
		},
	})
	if err != nil {
		log.Fatal(err)
	}
}
