package main

import (
	"flag"
	"fmt"
	"os"

	"ocrworkbench/internal/sample"
)

func main() {
	out := flag.String("out", "data", "output directory for deterministic samples and manifest.json")
	seed := flag.Int64("seed", 42, "deterministic generator seed")
	flag.Parse()
	manifest, err := sample.Generate(sample.Options{OutputDir: *out, Seed: *seed})
	if err != nil {
		fmt.Fprintln(os.Stderr, "ocrgen:", err)
		os.Exit(1)
	}
	fmt.Printf("generated %d images in %s (seed=%d template=%s truth=%s)\n",
		len(manifest.Images), *out, manifest.Seed, manifest.TemplateVersion, manifest.TruthFingerprint)
}
