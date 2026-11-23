//go:build !fyne

package main

import "fmt"

func main() {
	fmt.Println("The Fyne panel is isolated behind the 'fyne' build tag.")
	fmt.Println("Install the native Fyne prerequisites, then run:")
	fmt.Println("  go run -tags fyne ./cmd/glyphbrush-ui -samples ./samples")
}
