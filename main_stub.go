//go:build !wailsui && !windows

package main

import "fmt"

func main() {
	fmt.Println("Wails UI is optional. Build with: wails build -tags wailsui -s")
	fmt.Println("Run deterministic verification with: terrainctl verify -project samples/terrain_project.json")
}
