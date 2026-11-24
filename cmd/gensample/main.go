package main

import (
	"encoding/json"
	"flag"
	"fmt"
	"os"

	"screenverify/internal/sample"
)

func main() {
	out := flag.String("out", "data/sample", "local output directory for deterministic frames and truth")
	seed := flag.Int64("seed", 0x51A7E, "deterministic generator seed")
	frames := flag.Int("frames", sample.FrameCount, "frames per clip")
	flag.Parse()

	manifest, err := sample.GenerateDataset(sample.Options{OutputDir: *out, Seed: *seed, Frames: *frames})
	if err != nil {
		fmt.Fprintln(os.Stderr, "E_GENERATE:", err)
		os.Exit(1)
	}
	b, _ := json.MarshalIndent(manifest, "", "  ")
	fmt.Println(string(b))
	fmt.Fprintf(os.Stderr, "generated %d clips in %s\n", len(manifest.Clips), *out)
}
