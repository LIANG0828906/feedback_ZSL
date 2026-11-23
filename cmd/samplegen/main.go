package main

import (
	"flag"
	"fmt"
	"os"

	"stitchworkbench/internal/samplegen"
)

func main() {
	out := flag.String("out", "data/samples", "output directory for deterministic sample sets")
	seed := flag.Int64("seed", 42, "fixed seed")
	sets := flag.Int("sets", 5, "number of sets to generate")
	flag.Parse()
	if err := samplegen.Generate(samplegen.Config{OutDir: *out, Seed: *seed, Sets: *sets}); err != nil {
		fmt.Fprintln(os.Stderr, "sample generation failed:", err)
		os.Exit(1)
	}
	fmt.Printf("generated %d deterministic sample sets in %s (seed=%d)\n", *sets, *out, *seed)
}
