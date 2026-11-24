package main

import (
	"flag"
	"fmt"
	"os"

	"vectorlab/engine"
)

func main() {
	out := flag.String("out", "artifacts/sample", "output directory")
	flag.Parse()
	manifest, err := engine.GenerateSample(*out)
	if err != nil {
		fmt.Fprintln(os.Stderr, "scenegen:", err)
		os.Exit(1)
	}
	fmt.Printf("generated %d scene versions and truth in %s (seed=%x)\n", len(manifest.SceneFiles), *out, manifest.Seed)
}
