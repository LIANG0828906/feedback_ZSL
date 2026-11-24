//go:build !gui

package main

import (
	"fmt"
	"os"
)

func main() {
	fmt.Fprintln(os.Stderr, "The desktop build requires Fyne native dependencies.")
	fmt.Fprintln(os.Stderr, "Build with: go build -tags gui -o workspace-desktop ./cmd/desktop")
	fmt.Fprintln(os.Stderr, "The CLI remains available via: go run ./cmd/workspace")
}
