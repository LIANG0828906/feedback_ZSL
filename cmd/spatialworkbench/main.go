//go:build fyne || windows

package main

import "spatialworkbench/internal/desktop"

func main() {
	desktop.Run()
}
