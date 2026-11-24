//go:build !wails

package main

import "fmt"

func main() {
	fmt.Println("Resource Distribution Workbench")
	fmt.Println("CLI: go run ./cmd/workbench")
	fmt.Println("Desktop: wails build -tags wails")
	fmt.Println("See README.md for the local deterministic validation flow.")
}
