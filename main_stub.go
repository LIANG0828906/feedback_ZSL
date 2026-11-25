//go:build !windows

package main

import "fmt"

func main() {
	fmt.Println("Atelier desktop target is Windows/WebView2.")
	fmt.Println("Run the deterministic verification harness with: go run ./cmd/atelier-cli -verify")
}
