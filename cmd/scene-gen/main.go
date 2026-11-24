package main

import (
	"flag"
	"fmt"
	"os"
	"path/filepath"

	"occlusionlab/internal/generator"
	"occlusionlab/internal/model"
)

func main() {
	seed := flag.Int64("seed", 1042, "deterministic scene seed")
	out := flag.String("out", "data/projects/sample", "project output directory")
	count := flag.Int("count", 1, "number of consecutive seeds to generate")
	worldBits := flag.Uint("world-bits", 8, "world fixed-point fraction bits")
	basisBits := flag.Uint("basis-bits", 16, "camera basis fraction bits")
	screenBits := flag.Uint("screen-bits", 8, "screen fraction bits")
	paramBits := flag.Uint("param-bits", 16, "frustum parameter fraction bits")
	tolerancePx := flag.Float64("tolerance-px", 2, "declared projection tolerance in pixels")
	flag.Parse()

	for i := 0; i < *count; i++ {
		currentSeed := *seed + int64(i)
		cfg := generator.DefaultConfig(currentSeed)
		cfg.Quantization = model.Quantization{
			WorldFracBits:  uint8(*worldBits),
			BasisFracBits:  uint8(*basisBits),
			ScreenFracBits: uint8(*screenBits),
			ParamFracBits:  uint8(*paramBits),
		}
		cfg.TolerancePxQ = int64(*tolerancePx * float64(int64(1)<<cfg.Quantization.ScreenFracBits))
		dir := *out
		if *count > 1 {
			dir = filepath.Join(*out, fmt.Sprintf("seed-%d", currentSeed))
		}
		project, err := generator.GenerateToDir(dir, cfg)
		if err != nil {
			fmt.Fprintln(os.Stderr, err)
			os.Exit(1)
		}
		visible, hidden := 0, 0
		for _, sample := range project.Truth.Samples {
			for _, chunk := range sample.Chunks {
				if chunk.Visible {
					visible++
				} else {
					hidden++
				}
			}
		}
		fmt.Printf("seed=%d dir=%s chunks=%d windows=%d truth_visible=%d truth_hidden=%d hash=%s\n",
			currentSeed, dir, len(project.World.Chunks), len(project.World.Cameras),
			visible, hidden, project.ContentHash[:12])
	}
}
