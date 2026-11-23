package main

import (
	"flag"
	"fmt"
	"os"

	"ocrworkbench/internal/domain"
	"ocrworkbench/internal/synth"
)

func main() {
	dir := flag.String("dir", "samples", "output directory for deterministic samples")
	seed := flag.Int64("seed", domain.DefaultBaseSeed, "base random seed")
	flag.Parse()
	manifest, err := synth.Generate(*dir, *seed)
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	fmt.Printf("generated %d samples in %s (seed=%d, font=%s)\n", len(manifest.Samples), *dir, manifest.Seed, manifest.FontVersion)
	for _, sample := range manifest.Samples {
		fmt.Printf("  %-26s %4dx%-4d ch=%d lines=%d hash=%s\n",
			sample.ID, sample.Width, sample.Height, sample.Channels, sample.LineCount, sample.ImageHash[:12])
	}
}
