//go:build !wails

package main

import "fmt"

func main() {
	fmt.Println("ForensicLab desktop shell is built with: wails build -tags wails -s")
	fmt.Println("Headless verification: go run ./cmd/forensiclab verify --help")
}
