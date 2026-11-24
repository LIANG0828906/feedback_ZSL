package main

import (
	"flag"
	"fmt"
	"os"

	"volumediag/internal/scene"
)

func main() {
	opts := scene.DefaultGenerateOptions()
	out := flag.String("out", "artifacts/sample", "output directory")
	flag.Int64Var(&opts.Seed, "seed", opts.Seed, "deterministic seed")
	flag.IntVar(&opts.Width, "width", opts.Width, "viewport width")
	flag.IntVar(&opts.Height, "height", opts.Height, "viewport height")
	flag.IntVar(&opts.BlockWidth, "block-width", opts.BlockWidth, "block width")
	flag.IntVar(&opts.BlockHeight, "block-height", opts.BlockHeight, "block height")
	flag.IntVar(&opts.ReferenceSteps, "reference-steps", opts.ReferenceSteps, "truth reference integration steps")
	flag.Parse()

	bundle, err := scene.Generate(*out, opts)
	if err != nil {
		fmt.Fprintln(os.Stderr, "generate:", err)
		os.Exit(1)
	}
	fmt.Printf("generated %s\n", *out)
	fmt.Printf("source_hash=%s\n", bundle.SourceHash)
	fmt.Printf("scene_hash=%s\n", bundle.SceneHash)
	fmt.Printf("pixels=%d probes=%d\n", len(bundle.Truth.Pixels), len(bundle.Truth.Rays))
}
