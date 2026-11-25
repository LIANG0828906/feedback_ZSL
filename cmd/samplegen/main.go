package main

import (
	"flag"
	"fmt"
	"os"

	"example.com/vectorcore"
	"example.com/vectorcore/internal/samplegen"
)

func main() {
	out := flag.String("out", "samples", "output sample directory")
	seed := flag.Int64("seed", 20261125, "deterministic generator seed")
	batches := flag.Int("batches", 3, "number of scene batches")
	grid := flag.Int("grid", 4, "subsample grid")
	tolerance := flag.Int64("tolerance", 256, "flattening tolerance in 1/4096 logical pixels")
	flag.Parse()

	cfg := vectorcore.DefaultConfig()
	cfg.Grid = *grid
	cfg.Tolerance = *tolerance
	cfg.MaxWorkers = 1
	manifest, err := samplegen.Generate(samplegen.Options{
		Seed: *seed, Batches: *batches, OutDir: *out, Config: cfg,
	})
	if err != nil {
		fmt.Fprintf(os.Stderr, "sample generation failed: %v\n", err)
		os.Exit(1)
	}
	fmt.Printf("generated %d batches at %s (seed=%d, versions=1,2,3)\n", len(manifest.Batches), *out, *seed)
}
