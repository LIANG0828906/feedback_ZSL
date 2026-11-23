//go:build headless

package main

import (
	"fmt"
	"os"
)

func main() {
	fmt.Println("StageCue Workbench domain build is available.")
	fmt.Println("Use `go run ./cmd/stagecue --help` for generation, scheduling and verification.")
	fmt.Println("Build the Wails desktop UI with: wails build")
	if len(os.Args) > 1 {
		fmt.Fprintf(os.Stderr, "headless build ignores arguments: %v\n", os.Args[1:])
	}
}
