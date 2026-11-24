//go:build !fyne && !windows

package main

import "fmt"

func main() {
	fmt.Println("This command is the Fyne desktop workbench.")
	fmt.Println("Build and run it with: go run -tags fyne ./cmd/spatialworkbench")
	fmt.Println("The headless diagnostics CLI is: go run ./cmd/workbench help")
}
